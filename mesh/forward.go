package mesh

import (
	"fmt"
	"net"
)

const (
	meshHeaderLen = 9 // 4 (dst) + 4 (src) + 1 (ttl)
	defaultTTL    = 64
)

// encodeMeshFrame builds a FrameMeshPacket payload:
//
//	DST(4) + SRC(4) + TTL(1) + IP_PACKET
func encodeMeshFrame(dstVIP, srcVIP net.IP, ttl byte, ipPacket []byte) []byte {
	frame := make([]byte, meshHeaderLen+len(ipPacket))
	copy(frame[0:4], dstVIP.To4())
	copy(frame[4:8], srcVIP.To4())
	frame[8] = ttl
	copy(frame[meshHeaderLen:], ipPacket)
	return frame
}

// decodeMeshFrame parses a FrameMeshPacket payload.
func decodeMeshFrame(data []byte) (dstVIP, srcVIP net.IP, ttl byte, ipPacket []byte, err error) {
	if len(data) < meshHeaderLen {
		return nil, nil, 0, nil, fmt.Errorf("mesh frame too short: %d", len(data))
	}
	dstVIP = net.IP(make([]byte, 4))
	copy(dstVIP, data[0:4])
	srcVIP = net.IP(make([]byte, 4))
	copy(srcVIP, data[4:8])
	ttl = data[8]
	ipPacket = data[meshHeaderLen:]
	return
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

// reencodeMeshFrame updates TTL in an existing mesh frame for forwarding.
func decrementTTL(data []byte) byte {
	if len(data) < meshHeaderLen {
		return 0
	}
	data[8]--
	return data[8]
}
