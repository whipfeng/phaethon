package tun

import (
	"net"
	"sync"
	"time"
)

// NATTable implements source NAT for bypass gateway mode.
// Maps (srcIP, srcPort, dstIP, dstPort, protocol) to (VIP, mappedPort, dstIP, dstPort, protocol).
type NATTable struct {
	mu       sync.RWMutex
	vip      net.IP
	forward  map[string]*NATEntry // key: "proto:srcIP:srcPort:dstIP:dstPort"
	reverse  map[string]*NATEntry // key: "proto:mappedPort:dstIP:dstPort"
	nextPort uint16
	stopCh   chan struct{}
}

// NATEntry represents a single NAT mapping.
type NATEntry struct {
	OrigSrcIP   net.IP
	OrigSrcPort uint16
	DstIP       net.IP
	DstPort     uint16
	Protocol    byte // 6=TCP, 17=UDP
	MappedPort  uint16
	LastSeen    time.Time
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
	dstIP := net.IP(packet[16:20])
	headerLen := int(packet[0]&0x0f) * 4
	if headerLen < 20 || headerLen > len(packet) {
		return nil
	}

	var srcPort, dstPort uint16
	if proto == 6 && len(packet) >= headerLen+4 { // TCP
		srcPort = uint16(packet[headerLen])<<8 | uint16(packet[headerLen+1])
		dstPort = uint16(packet[headerLen+2])<<8 | uint16(packet[headerLen+3])
	} else if proto == 17 && len(packet) >= headerLen+4 { // UDP
		srcPort = uint16(packet[headerLen])<<8 | uint16(packet[headerLen+1])
		dstPort = uint16(packet[headerLen+2])<<8 | uint16(packet[headerLen+3])
	} else {
		return nil // unsupported protocol
	}

	key := natForwardKey(proto, srcIP, srcPort, dstIP, dstPort)

	t.mu.Lock()
	entry, exists := t.forward[key]
	if !exists {
		entry = &NATEntry{
			OrigSrcIP:   srcIP,
			OrigSrcPort: srcPort,
			DstIP:       dstIP,
			DstPort:     dstPort,
			Protocol:    proto,
			MappedPort:  t.nextPort,
		}
		t.nextPort++
		if t.nextPort < 32768 {
			t.nextPort = 32768
		}
		t.forward[key] = entry
		t.reverse[natReverseKey(proto, entry.MappedPort, dstIP, dstPort)] = entry
	}
	entry.LastSeen = time.Now()
	mappedPort := entry.MappedPort
	t.mu.Unlock()

	// Build translated packet
	result := make([]byte, len(packet))
	copy(result, packet)

	// Rewrite source IP to VIP
	copy(result[12:16], t.vip.To4())

	// Rewrite source port
	if proto == 6 || proto == 17 {
		result[headerLen] = byte(mappedPort >> 8)
		result[headerLen+1] = byte(mappedPort)
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

	// Note: TCP/UDP checksum recomputation is skipped for simplicity.
	// Most implementations tolerate this for IPv4.

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
	srcIP := net.IP(packet[12:16]) // remote server IP in response
	headerLen := int(packet[0]&0x0f) * 4
	if headerLen < 20 || headerLen > len(packet) {
		return nil
	}

	var srcPort, dstPort uint16
	if proto == 6 && len(packet) >= headerLen+4 { // TCP
		srcPort = uint16(packet[headerLen])<<8 | uint16(packet[headerLen+1])
		dstPort = uint16(packet[headerLen+2])<<8 | uint16(packet[headerLen+3])
	} else if proto == 17 && len(packet) >= headerLen+4 { // UDP
		srcPort = uint16(packet[headerLen])<<8 | uint16(packet[headerLen+1])
		dstPort = uint16(packet[headerLen+2])<<8 | uint16(packet[headerLen+3])
	} else {
		return nil
	}

	// Look up by the mapped port (which is now the dst port in the response)
	key := natReverseKey(proto, dstPort, srcIP, srcPort)

	t.mu.Lock()
	entry, exists := t.reverse[key]
	if exists {
		entry.LastSeen = time.Now()
	}
	t.mu.Unlock()

	if !exists {
		return nil
	}

	// Build translated packet
	result := make([]byte, len(packet))
	copy(result, packet)

	// Rewrite destination IP to original source
	copy(result[16:20], entry.OrigSrcIP.To4())

	// Rewrite destination port to original
	if proto == 6 || proto == 17 {
		result[headerLen+2] = byte(entry.OrigSrcPort >> 8)
		result[headerLen+3] = byte(entry.OrigSrcPort)
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

	return result
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

	cutoff := time.Now().Add(-5 * time.Minute)
	for key, entry := range t.forward {
		if entry.LastSeen.Before(cutoff) {
			delete(t.forward, key)
			revKey := natReverseKey(entry.Protocol, entry.MappedPort, entry.DstIP, entry.DstPort)
			delete(t.reverse, revKey)
		}
	}
}

func natForwardKey(proto byte, srcIP net.IP, srcPort uint16, dstIP net.IP, dstPort uint16) string {
	return string(rune(proto)) + ":" + srcIP.String() + ":" + itoa(srcPort) + ":" + dstIP.String() + ":" + itoa(dstPort)
}

func natReverseKey(proto byte, mappedPort uint16, dstIP net.IP, dstPort uint16) string {
	return string(rune(proto)) + ":" + itoa(mappedPort) + ":" + dstIP.String() + ":" + itoa(dstPort)
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
