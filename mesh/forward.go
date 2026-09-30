package mesh

import (
	"net"
	"phaethon/util"
)

// Mesh v2: raw IP packets are sent directly over P2P, no mesh frame header.
// The IP packet's own TTL field is used for loop prevention.

const defaultTTL = 64

// meshCIDR is the address range used for mesh addressing.
// Default: 100.64.0.0/10 (CGNAT range). Can be changed via SetMeshCIDR.
var meshCIDR *net.IPNet

func init() {
	_, meshCIDR, _ = net.ParseCIDR("100.64.0.0/10")
}

// SetMeshCIDR sets the overall mesh network range (e.g., "100.0.0.0/8").
// Must be called before mesh starts. Returns error if cidr is invalid.
func SetMeshCIDR(cidr string) error {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return err
	}
	meshCIDR = network
	util.LogInfo("[MESH] meshCIDR set to %s", cidr)
	return nil
}

// GetMeshCIDR returns the current mesh network range.
func GetMeshCIDR() *net.IPNet {
	return meshCIDR
}

// isMeshAddress reports whether the IP is in the mesh address range.
func isMeshAddress(ip net.IP) bool {
	return meshCIDR.Contains(ip)
}

// extractDstIP extracts the destination IP from a raw IPv4 packet.
func extractDstIP(ipPacket []byte) net.IP {
	if len(ipPacket) < 20 || ipPacket[0]>>4 != 4 {
		return nil
	}
	return net.IP(ipPacket[16:20])
}

// extractSrcIP extracts the source IP from a raw IPv4 packet.
func extractSrcIP(ipPacket []byte) net.IP {
	if len(ipPacket) < 16 || ipPacket[0]>>4 != 4 {
		return nil
	}
	return net.IP(ipPacket[12:16])
}

// DecrementIPTTL decrements the TTL in a raw IPv4 packet in-place.
// Returns the new TTL value, or 0 if TTL was already 0 or packet is invalid.
func DecrementIPTTL(ipPacket []byte) byte {
	if len(ipPacket) < 9 || ipPacket[0]>>4 != 4 {
		return 0
	}
	if ipPacket[8] == 0 {
		return 0 // TTL already 0, cannot decrement
	}
	ipPacket[8]--
	// Recompute header checksum
	ipPacket[10] = 0
	ipPacket[11] = 0
	var sum uint32
	headerLen := int(ipPacket[0]&0x0f) * 4
	if headerLen < 20 || headerLen > len(ipPacket) {
		headerLen = 20
	}
	for i := 0; i < headerLen-1; i += 2 {
		sum += uint32(ipPacket[i])<<8 | uint32(ipPacket[i+1])
	}
	for sum>>16 > 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	cksum := ^uint16(sum)
	ipPacket[10] = byte(cksum >> 8)
	ipPacket[11] = byte(cksum)
	return ipPacket[8]
}

// GenerateICMPTimeExceeded creates an ICMP Time Exceeded message for the given IP packet.
// The ICMP packet has src=localVIP and dst=original source IP.
// Returns the complete ICMP packet (IP header + ICMP header + original IP packet as payload).
func GenerateICMPTimeExceeded(localVIP net.IP, originalPacket []byte) []byte {
	if len(originalPacket) < 20 || localVIP == nil {
		return nil
	}

	// ICMP Time Exceeded payload: original IP header + at least 8 bytes of original data
	payloadLen := len(originalPacket)
	if payloadLen > 576-20-8 { // Max ICMP packet size 576, minus IP header and ICMP header
		payloadLen = 576 - 20 - 8
	}

	// Build ICMP packet: IP header (20 bytes) + ICMP header (8 bytes) + payload
	icmpPacket := make([]byte, 20+8+payloadLen)

	// IP header (20 bytes)
	icmpPacket[0] = 0x45 // Version=4, IHL=5 (20 bytes)
	icmpPacket[1] = 0    // TOS
	totalLen := len(icmpPacket)
	icmpPacket[2] = byte(totalLen >> 8)
	icmpPacket[3] = byte(totalLen)
	icmpPacket[4] = 0 // Identification
	icmpPacket[5] = 0
	icmpPacket[6] = 0 // Flags + Fragment offset
	icmpPacket[7] = 0
	icmpPacket[8] = 64 // TTL
	icmpPacket[9] = 1  // Protocol = ICMP

	// Source IP = local VIP
	copy(icmpPacket[12:16], localVIP.To4())
	// Destination IP = original packet's source
	copy(icmpPacket[16:20], originalPacket[12:16])

	// Compute IP header checksum
	icmpPacket[10] = 0
	icmpPacket[11] = 0
	var ipSum uint32
	for i := 0; i < 20-1; i += 2 {
		ipSum += uint32(icmpPacket[i])<<8 | uint32(icmpPacket[i+1])
	}
	for ipSum>>16 > 0 {
		ipSum = (ipSum & 0xffff) + (ipSum >> 16)
	}
	ipCksum := ^uint16(ipSum)
	icmpPacket[10] = byte(ipCksum >> 8)
	icmpPacket[11] = byte(ipCksum)

	// ICMP header (8 bytes)
	icmpPacket[20] = 11 // Type = Time Exceeded
	icmpPacket[21] = 0  // Code = TTL exceeded
	icmpPacket[22] = 0  // Checksum (computed later)
	icmpPacket[23] = 0  // Unused

	// Payload: original IP packet (truncated if necessary)
	copy(icmpPacket[28:], originalPacket[:payloadLen])

	// Compute ICMP checksum (over ICMP header + payload)
	icmpPacket[22] = 0
	icmpPacket[23] = 0
	var icmpSum uint32
	for i := 20; i < len(icmpPacket)-1; i += 2 {
		icmpSum += uint32(icmpPacket[i])<<8 | uint32(icmpPacket[i+1])
	}
	if len(icmpPacket)%2 == 1 {
		icmpSum += uint32(icmpPacket[len(icmpPacket)-1]) << 8
	}
	for icmpSum>>16 > 0 {
		icmpSum = (icmpSum & 0xffff) + (icmpSum >> 16)
	}
	icmpCksum := ^uint16(icmpSum)
	icmpPacket[22] = byte(icmpCksum >> 8)
	icmpPacket[23] = byte(icmpCksum)

	return icmpPacket
}
