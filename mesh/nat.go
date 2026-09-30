package mesh

import (
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// NATTable implements source NAT for bypass gateway mode.
// Forward key: (proto, srcIP, srcPort) → entry with allocated mappedPort.
// Reverse key: (proto, mappedPort) → entry with original srcIP and srcPort.
type NATTable struct {
	mu       sync.RWMutex
	vip      net.IP
	forward  map[string]*NATEntry // key: "proto:srcIP:srcPort"
	reverse  map[string]*NATEntry // key: "proto:mappedPort"
	nextPort uint16
	stopCh   chan struct{}
}

// NATEntry represents a single NAT mapping.
type NATEntry struct {
	OrigSrcIP   net.IP
	OrigSrcPort uint16
	Protocol    byte // 6=TCP, 17=UDP
	MappedPort  uint16
	LastSeen    atomic.Int64 // Unix nano
}

// NewNATTable creates a NAT table with the given VIP as the source address.
func NewNATTable(vip net.IP) *NATTable {
	t := &NATTable{
		vip:      vip.To4(),
		forward:  make(map[string]*NATEntry),
		reverse:  make(map[string]*NATEntry),
		nextPort: 32768,
		stopCh:   make(chan struct{}),
	}
	go t.cleanupLoop()
	return t
}

// Stop stops the NAT table cleanup goroutine.
func (t *NATTable) Stop() {
	close(t.stopCh)
}

// TranslateOutbound performs source NAT on an outbound packet.
// Rewrites srcIP to VIP and returns the modified packet.
// Returns nil if the packet cannot be translated.
func (t *NATTable) TranslateOutbound(packet []byte) []byte {
	if len(packet) < 20 {
		return nil
	}
	if packet[0]>>4 != 4 {
		return nil
	}

	proto := packet[9]
	srcIP := net.IP(packet[12:16])
	headerLen := int(packet[0]&0x0f) * 4
	if headerLen < 20 || headerLen > len(packet) {
		return nil
	}

	var srcPort uint16
	if proto == 6 && len(packet) >= headerLen+4 { // TCP
		srcPort = uint16(packet[headerLen])<<8 | uint16(packet[headerLen+1])
	} else if proto == 17 && len(packet) >= headerLen+4 { // UDP
		srcPort = uint16(packet[headerLen])<<8 | uint16(packet[headerLen+1])
	} else if proto == 1 && len(packet) >= headerLen+8 { // ICMP (Echo Request/Reply)
		// ICMP ID is at offset 4-5 in ICMP header (after type, code, checksum)
		srcPort = uint16(packet[headerLen+4])<<8 | uint16(packet[headerLen+5])
	} else {
		return nil // unsupported protocol
	}

	key := natForwardKey(proto, srcIP, srcPort)

	var mappedPort uint16

	t.mu.RLock()
	entry, exists := t.forward[key]
	t.mu.RUnlock()

	if exists {
		mappedPort = entry.MappedPort
	} else {
		t.mu.Lock()
		entry, exists = t.forward[key]
		if !exists {
			entry = &NATEntry{
				OrigSrcIP:   srcIP,
				OrigSrcPort: srcPort,
				Protocol:    proto,
				MappedPort:  t.nextPort,
			}
			t.nextPort++
			if t.nextPort < 32768 {
				t.nextPort = 32768
			}
			t.forward[key] = entry
			t.reverse[natReverseKey(proto, entry.MappedPort)] = entry
		}
		mappedPort = entry.MappedPort
		t.mu.Unlock()
	}

	entry.LastSeen.Store(time.Now().UnixNano())

	// Build translated packet
	result := make([]byte, len(packet))
	copy(result, packet)

	// Rewrite source IP to VIP
	copy(result[12:16], t.vip.To4())

	// Rewrite source port (or ICMP ID)
	if proto == 6 || proto == 17 {
		result[headerLen] = byte(mappedPort >> 8)
		result[headerLen+1] = byte(mappedPort)
	} else if proto == 1 {
		// Rewrite ICMP ID
		result[headerLen+4] = byte(mappedPort >> 8)
		result[headerLen+5] = byte(mappedPort)
		// Recompute ICMP checksum
		recomputeICMPChecksum(result, headerLen)
	}

	// Recompute IP header checksum
	result[10] = 0
	result[11] = 0
	var sum uint32
	for i := 0; i < headerLen-1; i += 2 {
		sum += uint32(result[i])<<8 | uint32(result[i+1])
	}
	for sum>>16 > 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	cksum := ^uint16(sum)
	result[10] = byte(cksum >> 8)
	result[11] = byte(cksum)

	// Recompute TCP/UDP checksum for srcIP and srcPort changes
	recomputeTCPUDPChecksum(result, headerLen, proto, net.IP(result[12:16]), net.IP(result[16:20]))

	return result
}

// TranslateInbound performs reverse NAT on an inbound packet.
// Rewrites dstIP from VIP back to original srcIP.
// Returns nil if no mapping exists.
func (t *NATTable) TranslateInbound(packet []byte) []byte {
	if len(packet) < 20 {
		return nil
	}
	if packet[0]>>4 != 4 {
		return nil
	}

	proto := packet[9]
	headerLen := int(packet[0]&0x0f) * 4
	if headerLen < 20 || headerLen > len(packet) {
		return nil
	}

	var dstPort uint16
	if proto == 6 && len(packet) >= headerLen+4 { // TCP
		dstPort = uint16(packet[headerLen+2])<<8 | uint16(packet[headerLen+3])
	} else if proto == 17 && len(packet) >= headerLen+4 { // UDP
		dstPort = uint16(packet[headerLen+2])<<8 | uint16(packet[headerLen+3])
	} else if proto == 1 && len(packet) >= headerLen+8 { // ICMP
		icmpType := packet[headerLen]
		// ICMP error messages (type 3, 11, 12) contain embedded original packet
		if icmpType == 3 || icmpType == 11 || icmpType == 12 {
			return t.translateICMPError(packet, headerLen)
		}
		// ICMP Echo Request/Reply: ID is at offset 4-5 in ICMP header
		dstPort = uint16(packet[headerLen+4])<<8 | uint16(packet[headerLen+5])
	} else {
		return nil
	}

	// Look up by the mapped port (which is now the dst port in the response)
	key := natReverseKey(proto, dstPort)

	t.mu.RLock()
	entry, exists := t.reverse[key]
	t.mu.RUnlock()

	if !exists {
		return nil
	}

	entry.LastSeen.Store(time.Now().UnixNano())

	// Build translated packet
	result := make([]byte, len(packet))
	copy(result, packet)

	// Rewrite destination IP to original source
	copy(result[16:20], entry.OrigSrcIP.To4())

	// Rewrite destination port to original
	if proto == 6 || proto == 17 {
		result[headerLen+2] = byte(entry.OrigSrcPort >> 8)
		result[headerLen+3] = byte(entry.OrigSrcPort)
	} else if proto == 1 {
		// Rewrite ICMP ID to original
		result[headerLen+4] = byte(entry.OrigSrcPort >> 8)
		result[headerLen+5] = byte(entry.OrigSrcPort)
		// Recompute ICMP checksum
		recomputeICMPChecksum(result, headerLen)
	}

	// Recompute IP header checksum
	result[10] = 0
	result[11] = 0
	var sum uint32
	for i := 0; i < headerLen-1; i += 2 {
		sum += uint32(result[i])<<8 | uint32(result[i+1])
	}
	for sum>>16 > 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	cksum := ^uint16(sum)
	result[10] = byte(cksum >> 8)
	result[11] = byte(cksum)

	// Recompute TCP/UDP checksum for dstIP and dstPort changes
	recomputeTCPUDPChecksum(result, headerLen, proto, net.IP(result[12:16]), net.IP(result[16:20]))

	return result
}

// translateICMPError handles reverse NAT for ICMP error messages (type 3, 11, 12).
// These contain the embedded original packet which we use to look up the NAT entry.
func (t *NATTable) translateICMPError(packet []byte, outerHeaderLen int) []byte {
	// ICMP error format: outer IP + ICMP header (8 bytes) + embedded IP header + embedded data
	icmpStart := outerHeaderLen
	embeddedStart := icmpStart + 8 // After ICMP type/code/checksum/unused

	if len(packet) < embeddedStart+20 {
		return nil // Not enough data for embedded IP header
	}

	// Parse embedded IP header
	embeddedIP := packet[embeddedStart:]
	if embeddedIP[0]>>4 != 4 {
		return nil // Not IPv4
	}
	embeddedHeaderLen := int(embeddedIP[0]&0x0f) * 4
	if embeddedHeaderLen < 20 || embeddedStart+embeddedHeaderLen > len(packet) {
		return nil
	}

	embeddedProto := embeddedIP[9]

	// Extract embedded source port (the mapped port from forward NAT)
	var embeddedSrcPort uint16
	if embeddedProto == 6 && len(embeddedIP) >= embeddedHeaderLen+4 { // TCP
		embeddedSrcPort = uint16(embeddedIP[embeddedHeaderLen])<<8 | uint16(embeddedIP[embeddedHeaderLen+1])
	} else if embeddedProto == 17 && len(embeddedIP) >= embeddedHeaderLen+4 { // UDP
		embeddedSrcPort = uint16(embeddedIP[embeddedHeaderLen])<<8 | uint16(embeddedIP[embeddedHeaderLen+1])
	} else if embeddedProto == 1 && len(embeddedIP) >= embeddedHeaderLen+8 { // ICMP
		embeddedSrcPort = uint16(embeddedIP[embeddedHeaderLen+4])<<8 | uint16(embeddedIP[embeddedHeaderLen+5])
	} else {
		return nil
	}

	// Look up NAT entry by embedded (proto, mappedPort)
	key := natReverseKey(embeddedProto, embeddedSrcPort)
	t.mu.RLock()
	entry, exists := t.reverse[key]
	t.mu.RUnlock()

	if !exists {
		return nil
	}

	entry.LastSeen.Store(time.Now().UnixNano())

	// Build translated packet
	result := make([]byte, len(packet))
	copy(result, packet)

	// Rewrite outer destination IP to original source
	copy(result[16:20], entry.OrigSrcIP.To4())

	// Rewrite embedded source IP to original source
	copy(result[embeddedStart+12:embeddedStart+16], entry.OrigSrcIP.To4())

	// Rewrite embedded source port to original
	if embeddedProto == 6 || embeddedProto == 17 {
		result[embeddedStart+embeddedHeaderLen] = byte(entry.OrigSrcPort >> 8)
		result[embeddedStart+embeddedHeaderLen+1] = byte(entry.OrigSrcPort)
	} else if embeddedProto == 1 {
		result[embeddedStart+embeddedHeaderLen+4] = byte(entry.OrigSrcPort >> 8)
		result[embeddedStart+embeddedHeaderLen+5] = byte(entry.OrigSrcPort)
	}

	// Recompute embedded transport layer checksum
	recomputeTCPUDPChecksum(result, embeddedStart+embeddedHeaderLen, embeddedProto,
		net.IP(result[embeddedStart+12:embeddedStart+16]), net.IP(result[embeddedStart+16:embeddedStart+20]))

	// Recompute outer ICMP checksum
	recomputeICMPChecksum(result, outerHeaderLen)

	// Recompute outer IP header checksum
	result[10] = 0
	result[11] = 0
	var sum uint32
	for i := 0; i < outerHeaderLen-1; i += 2 {
		sum += uint32(result[i])<<8 | uint32(result[i+1])
	}
	for sum>>16 > 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	cksum := ^uint16(sum)
	result[10] = byte(cksum >> 8)
	result[11] = byte(cksum)

	return result
}

// TranslateInboundWithSrc performs reverse NAT and also rewrites the source IP.
// Used by the mesh return path so responses appear to come from the local GIP.
func (t *NATTable) TranslateInboundWithSrc(packet []byte, newSrcIP net.IP) []byte {
	result := t.TranslateInbound(packet)
	if result == nil {
		return nil
	}

	headerLen := int(result[0]&0x0f) * 4
	proto := result[9]

	copy(result[12:16], newSrcIP.To4())

	// Recompute IP header checksum
	result[10] = 0
	result[11] = 0
	var sum uint32
	for i := 0; i < headerLen-1; i += 2 {
		sum += uint32(result[i])<<8 | uint32(result[i+1])
	}
	for sum>>16 > 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	cksum := ^uint16(sum)
	result[10] = byte(cksum >> 8)
	result[11] = byte(cksum)

	recomputeTCPUDPChecksum(result, headerLen, proto, newSrcIP.To4(), net.IP(result[16:20]))

	return result
}

// RewriteSrcIP rewrites only the source IP address of a packet,
// recomputing IP header and TCP/UDP checksums. Unlike TranslateInboundWithSrc,
// it does not perform any NAT port translation and always succeeds.
func (t *NATTable) RewriteSrcIP(packet []byte, newSrcIP net.IP) []byte {
	if len(packet) < 20 || packet[0]>>4 != 4 {
		return nil
	}

	result := make([]byte, len(packet))
	copy(result, packet)

	newSrc := newSrcIP.To4()
	copy(result[12:16], newSrc)

	headerLen := int(result[0]&0x0f) * 4
	if headerLen < 20 || headerLen > len(result) {
		return nil
	}

	// Recompute IP header checksum
	result[10] = 0
	result[11] = 0
	var sum uint32
	for i := 0; i < headerLen-1; i += 2 {
		sum += uint32(result[i])<<8 | uint32(result[i+1])
	}
	for sum>>16 > 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	cksum := ^uint16(sum)
	result[10] = byte(cksum >> 8)
	result[11] = byte(cksum)

	// Recompute TCP/UDP checksum for srcIP change
	proto := result[9]
	recomputeTCPUDPChecksum(result, headerLen, proto, newSrc, net.IP(result[16:20]))

	return result
}

// ResolveOriginalSrc reverse-looks up the original source address.
// If srcIP is the VIP, looks up the NAT reverse table by (proto, srcPort as mappedPort)
// and returns the original (srcIP, srcPort). Otherwise returns the input unchanged.
func (t *NATTable) ResolveOriginalSrc(proto byte, srcIP net.IP, srcPort uint16) (net.IP, uint16) {
	if !srcIP.Equal(t.vip) {
		return srcIP, srcPort
	}
	key := natReverseKey(proto, srcPort)
	t.mu.RLock()
	entry, exists := t.reverse[key]
	t.mu.RUnlock()
	if !exists {
		return srcIP, srcPort
	}
	return entry.OrigSrcIP, entry.OrigSrcPort
}

// Stats returns the number of active NAT entries.
func (t *NATTable) Stats() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.forward)
}

func (t *NATTable) cleanupLoop() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-t.stopCh:
			return
		case <-ticker.C:
			t.cleanup()
		}
	}
}

func (t *NATTable) cleanup() {
	t.mu.Lock()
	defer t.mu.Unlock()

	cutoff := time.Now().Add(-5 * time.Minute).UnixNano()
	for key, entry := range t.forward {
		if entry.LastSeen.Load() < cutoff {
			delete(t.forward, key)
			revKey := natReverseKey(entry.Protocol, entry.MappedPort)
			delete(t.reverse, revKey)
		}
	}
}

func natForwardKey(proto byte, srcIP net.IP, srcPort uint16) string {
	return string(rune(proto)) + ":" + srcIP.String() + ":" + itoa(srcPort)
}

func natReverseKey(proto byte, mappedPort uint16) string {
	return string(rune(proto)) + ":" + itoa(mappedPort)
}

func itoa(n uint16) string {
	if n == 0 {
		return "0"
	}
	var buf [5]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// recomputeICMPChecksum recomputes the ICMP checksum from scratch
// after NAT rewrites the ICMP ID.
func recomputeICMPChecksum(pkt []byte, ipHeaderLen int) {
	icmpStart := ipHeaderLen
	icmpLen := len(pkt) - icmpStart
	if icmpLen < 8 {
		return
	}

	// Zero out checksum field (offset 2-3 in ICMP header)
	pkt[icmpStart+2] = 0
	pkt[icmpStart+3] = 0

	// Calculate checksum over entire ICMP message
	var sum uint32
	for i := 0; i < icmpLen-1; i += 2 {
		sum += uint32(pkt[icmpStart+i])<<8 | uint32(pkt[icmpStart+i+1])
	}
	if icmpLen%2 != 0 {
		sum += uint32(pkt[icmpStart+icmpLen-1]) << 8
	}

	for sum>>16 > 0 {
		sum = (sum&0xffff + sum>>16)
	}
	cksum := ^uint16(sum)

	pkt[icmpStart+2] = byte(cksum >> 8)
	pkt[icmpStart+3] = byte(cksum)
}

// recomputeTCPUDPChecksum recomputes the TCP or UDP checksum from scratch
// after NAT rewrites IP addresses and/or ports.
func recomputeTCPUDPChecksum(pkt []byte, ipHeaderLen int, proto byte, srcIP, dstIP net.IP) {
	if proto != 6 && proto != 17 {
		return
	}
	tcpStart := ipHeaderLen
	segLen := len(pkt) - tcpStart
	if segLen < 8 {
		return
	}

	if proto == 6 {
		if segLen < 18 {
			return
		}
		pkt[tcpStart+16] = 0
		pkt[tcpStart+17] = 0
	} else {
		if segLen < 8 {
			return
		}
		pkt[tcpStart+6] = 0
		pkt[tcpStart+7] = 0
	}

	var sum uint32

	sum += uint32(srcIP[0])<<8 | uint32(srcIP[1])
	sum += uint32(srcIP[2])<<8 | uint32(srcIP[3])
	sum += uint32(dstIP[0])<<8 | uint32(dstIP[1])
	sum += uint32(dstIP[2])<<8 | uint32(dstIP[3])
	sum += uint32(proto)
	sum += uint32(segLen)

	for i := 0; i < segLen-1; i += 2 {
		sum += uint32(pkt[tcpStart+i])<<8 | uint32(pkt[tcpStart+i+1])
	}
	if segLen%2 != 0 {
		sum += uint32(pkt[tcpStart+segLen-1]) << 8
	}

	for sum>>16 > 0 {
		sum = (sum&0xffff + sum>>16)
	}
	cksum := ^uint16(sum)

	if proto == 6 {
		pkt[tcpStart+16] = byte(cksum >> 8)
		pkt[tcpStart+17] = byte(cksum)
	} else {
		pkt[tcpStart+6] = byte(cksum >> 8)
		pkt[tcpStart+7] = byte(cksum)
	}
}
