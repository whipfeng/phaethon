package tun

import "encoding/binary"

// overlayMSS is the largest TCP payload that fits the overlay MTU.
const overlayMSS = OverlayMTU - 40

// ClampTCPMSS lowers the MSS option of an IPv4 TCP SYN/SYN-ACK to overlayMSS
// and recomputes the TCP checksum. It reports whether the packet changed.
//
// A LAN client forwarding through a gateway node enters the TUN with the
// client's own MTU (1500, MSS 1460). The overlay only carries 1400-byte inner
// packets, so the peer's reply segments are dropped by the TUN write path and
// the handshake stalls. TLS 1.3 hits this immediately because its first flight
// is larger than TLS 1.2's.
func ClampTCPMSS(pkt []byte) bool {
	if len(pkt) < 40 || pkt[0]>>4 != 4 || pkt[9] != 6 {
		return false
	}
	ihl := int(pkt[0]&0x0f) * 4
	totalLen := int(binary.BigEndian.Uint16(pkt[2:4]))
	if ihl < 20 || totalLen < ihl+20 || totalLen > len(pkt) {
		return false
	}
	// Non-initial fragments have no TCP header.
	if binary.BigEndian.Uint16(pkt[6:8])&0x1fff != 0 {
		return false
	}
	seg := pkt[ihl:totalLen]
	doff := int(seg[12]>>4) * 4
	if doff < 20 || doff > len(seg) {
		return false
	}
	if seg[13]&0x02 == 0 { // SYN
		return false
	}
	for i := 20; i < doff; {
		switch seg[i] {
		case 0: // end of options
			return false
		case 1: // nop
			i++
			continue
		}
		if i+1 >= doff {
			return false
		}
		l := int(seg[i+1])
		if l < 2 || i+l > doff {
			return false
		}
		if seg[i] == 2 && l == 4 {
			if binary.BigEndian.Uint16(seg[i+2:i+4]) <= overlayMSS {
				return false
			}
			binary.BigEndian.PutUint16(seg[i+2:i+4], overlayMSS)
			seg[16], seg[17] = 0, 0
			binary.BigEndian.PutUint16(seg[16:18], tcpChecksum(pkt[12:20], seg))
			return true
		}
		i += l
	}
	return false
}

func tcpChecksum(pseudo, seg []byte) uint16 {
	var sum uint32
	sum = addChecksum(sum, pseudo)
	sum += 0x0006 // zero byte + IPPROTO_TCP
	sum += uint32(len(seg))
	sum = addChecksum(sum, seg)
	for sum > 0xffff {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

func addChecksum(sum uint32, b []byte) uint32 {
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	return sum
}
