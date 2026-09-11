package mesh

import (
	"net"
)

// Mesh v2: raw IP packets are sent directly over P2P, no mesh frame header.
// The IP packet's own TTL field is used for loop prevention.

const defaultTTL = 64

// meshCIDR is the 100.64.0.0/10 CGNAT range used for mesh addressing.
var meshCIDR *net.IPNet

func init() {
	_, meshCIDR, _ = net.ParseCIDR("100.64.0.0/10")
}

// isMeshAddress reports whether the IP is in the mesh address range (100.64.0.0/10).
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

// decrementIPTTL decrements the TTL in a raw IPv4 packet in-place.
// Returns the new TTL value, or 0 if the packet is invalid.
func decrementIPTTL(ipPacket []byte) byte {
	if len(ipPacket) < 9 || ipPacket[0]>>4 != 4 {
		return 0
	}
	if ipPacket[8] <= 1 {
		return 0
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
