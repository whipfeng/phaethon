package tun

import (
	"encoding/binary"
	"net"
	"testing"
)

func buildTCPPacket(t *testing.T, flags byte, mss uint16, withMSS bool, payload []byte) []byte {
	t.Helper()

	var opts []byte
	if withMSS {
		opts = []byte{2, 4, byte(mss >> 8), byte(mss), 1, 1, 0, 0}
	} else {
		opts = []byte{1, 1, 1, 0}
	}
	tcpLen := 20 + len(opts) + len(payload)
	pkt := make([]byte, 20+tcpLen)
	pkt[0] = 0x45
	binary.BigEndian.PutUint16(pkt[2:4], uint16(len(pkt)))
	pkt[8] = 64
	pkt[9] = 6
	copy(pkt[12:16], net.IPv4(192, 168, 1, 88).To4())
	copy(pkt[16:20], net.IPv4(100, 179, 0, 10).To4())

	seg := pkt[20:]
	binary.BigEndian.PutUint16(seg[0:2], 52748)
	binary.BigEndian.PutUint16(seg[2:4], 443)
	seg[12] = byte((20 + len(opts)) / 4 << 4)
	seg[13] = flags
	copy(seg[20:], opts)
	copy(seg[20+len(opts):], payload)
	binary.BigEndian.PutUint16(seg[16:18], refTCPChecksum(pkt[12:20], seg))
	return pkt
}

// refTCPChecksum is an independent implementation so the test does not simply
// re-run the production checksum routine.
func refTCPChecksum(pseudo, seg []byte) uint16 {
	var sum uint32
	sum = refAddWords(sum, pseudo)
	sum += 6
	sum += uint32(len(seg))
	sum = refAddWords(sum, seg)
	for sum > 0xffff {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

func refAddWords(sum uint32, b []byte) uint32 {
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	return sum
}

func mssOption(t *testing.T, pkt []byte) uint16 {
	t.Helper()
	ihl := int(pkt[0]&0x0f) * 4
	seg := pkt[ihl:]
	doff := int(seg[12]>>4) * 4
	for i := 20; i < doff; {
		switch seg[i] {
		case 0:
			return 0
		case 1:
			i++
			continue
		}
		l := int(seg[i+1])
		if l < 2 {
			return 0
		}
		if seg[i] == 2 && l == 4 {
			return binary.BigEndian.Uint16(seg[i+2 : i+4])
		}
		i += l
	}
	return 0
}

func TestClampTCPMSSSyn(t *testing.T) {
	pkt := buildTCPPacket(t, 0x02, 1460, true, nil)
	if !ClampTCPMSS(pkt) {
		t.Fatal("expected SYN MSS to be clamped")
	}
	if got := mssOption(t, pkt); got != overlayMSS {
		t.Fatalf("mss = %d, want %d", got, overlayMSS)
	}
	seg := pkt[20:]
	if got := refTCPChecksum(pkt[12:20], seg); got != 0 {
		t.Fatalf("tcp checksum invalid after clamp: %#x", got)
	}
}

func TestClampTCPMSSSynAck(t *testing.T) {
	pkt := buildTCPPacket(t, 0x12, 1400, true, nil)
	if !ClampTCPMSS(pkt) {
		t.Fatal("expected SYN-ACK MSS to be clamped")
	}
	if got := mssOption(t, pkt); got != overlayMSS {
		t.Fatalf("mss = %d, want %d", got, overlayMSS)
	}
	if got := refTCPChecksum(pkt[12:20], pkt[20:]); got != 0 {
		t.Fatalf("tcp checksum invalid after clamp: %#x", got)
	}
}

func TestClampTCPMSSAtLimitUnchanged(t *testing.T) {
	pkt := buildTCPPacket(t, 0x02, overlayMSS, true, nil)
	if ClampTCPMSS(pkt) {
		t.Fatal("MSS at the limit should not be rewritten")
	}
}

func TestClampTCPMSSIgnoresNonSyn(t *testing.T) {
	pkt := buildTCPPacket(t, 0x10, 1460, true, nil)
	if ClampTCPMSS(pkt) {
		t.Fatal("non-SYN packet should not be rewritten")
	}
}

func TestClampTCPMSSIgnoresMissingOption(t *testing.T) {
	pkt := buildTCPPacket(t, 0x02, 0, false, nil)
	if ClampTCPMSS(pkt) {
		t.Fatal("SYN without MSS option should not be rewritten")
	}
}

func TestClampTCPMSSIgnoresNonIPv4OrTCP(t *testing.T) {
	pkt := buildTCPPacket(t, 0x02, 1460, true, nil)
	pkt[0] = 0x65 // IPv6
	if ClampTCPMSS(pkt) {
		t.Fatal("IPv6 packet should be ignored")
	}
	pkt = buildTCPPacket(t, 0x02, 1460, true, nil)
	pkt[9] = 17 // UDP
	if ClampTCPMSS(pkt) {
		t.Fatal("non-TCP packet should be ignored")
	}
	if ClampTCPMSS(pkt[:30]) {
		t.Fatal("truncated packet should be ignored")
	}
}

func TestClampTCPMSSIgnoresFragment(t *testing.T) {
	pkt := buildTCPPacket(t, 0x02, 1460, true, nil)
	pkt[7] = 0x01 // non-zero fragment offset (8-byte units)
	if ClampTCPMSS(pkt) {
		t.Fatal("fragment should be ignored")
	}
}

func TestClampTCPMSSKeepsPayloadChecksum(t *testing.T) {
	pkt := buildTCPPacket(t, 0x02, 1460, true, []byte("hello"))
	if !ClampTCPMSS(pkt) {
		t.Fatal("expected SYN MSS to be clamped")
	}
	if got := refTCPChecksum(pkt[12:20], pkt[20:]); got != 0 {
		t.Fatalf("tcp checksum invalid with payload: %#x", got)
	}
}
