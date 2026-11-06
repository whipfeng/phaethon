package frame

import "testing"

func ipv4(proto byte, src, dst [4]byte, l4 []byte) []byte {
	p := make([]byte, 20+len(l4))
	p[0] = 0x45
	p[9] = proto
	copy(p[12:16], src[:])
	copy(p[16:20], dst[:])
	copy(p[20:], l4)
	return p
}

func tcpSeg(sport, dport uint16, payload int) []byte {
	s := make([]byte, 20+payload)
	s[0], s[1] = byte(sport>>8), byte(sport)
	s[2], s[3] = byte(dport>>8), byte(dport)
	s[12] = 5 << 4
	s[13] = 0x10
	return s
}

func ipip(inner []byte) []byte {
	return ipv4(4, [4]byte{100, 2, 0, 1}, [4]byte{100, 64, 0, 1}, inner)
}

func TestInnerPacket_IPIP(t *testing.T) {
	inner := ipv4(6, [4]byte{172, 30, 0, 2}, [4]byte{10, 193, 131, 25}, tcpSeg(40000, 443, 0))
	pkt := ipip(inner)
	got := InnerPacket(pkt)
	if len(got) != len(inner) || &got[0] != &pkt[20] {
		t.Fatalf("InnerPacket did not return inner slice")
	}
}

func TestInnerPacket_Fallbacks(t *testing.T) {
	inner := ipv4(6, [4]byte{172, 30, 0, 2}, [4]byte{10, 0, 0, 1}, tcpSeg(1, 2, 0))
	cases := map[string][]byte{
		"plain tcp": inner,
		"empty":     {},
		"short":     ipip(inner)[:19],
		"truncated": ipip(inner)[:30],
	}
	mf := ipip(inner)
	mf[6] = 0x20
	cases["more fragments"] = mf
	off := ipip(inner)
	off[7] = 1
	cases["fragment offset"] = off
	v6 := ipip(inner)
	v6[20] = 0x60
	cases["inner not ipv4"] = v6
	bad := ipip(inner)
	bad[0] = 0x44
	cases["bad ihl"] = bad
	for name, p := range cases {
		got := InnerPacket(p)
		if len(got) != len(p) || (len(p) > 0 && &got[0] != &p[0]) {
			t.Errorf("%s: expected fallback to outer packet", name)
		}
	}
}

func TestFlowKey_IPIPUsesInnerFlow(t *testing.T) {
	a1 := ipip(ipv4(6, [4]byte{172, 30, 0, 2}, [4]byte{10, 0, 0, 1}, tcpSeg(40000, 443, 100)))
	a2 := ipip(ipv4(6, [4]byte{172, 30, 0, 2}, [4]byte{10, 0, 0, 1}, tcpSeg(40000, 443, 0)))
	b := ipip(ipv4(6, [4]byte{172, 30, 0, 2}, [4]byte{10, 0, 0, 1}, tcpSeg(40001, 443, 100)))
	if FlowKey(a1) != FlowKey(a2) {
		t.Fatal("same inner flow got different keys")
	}
	if FlowKey(a1) == FlowKey(b) {
		t.Fatal("different inner flows collapsed onto one key")
	}
	inner := ipv4(6, [4]byte{172, 30, 0, 2}, [4]byte{10, 0, 0, 1}, tcpSeg(40000, 443, 100))
	if FlowKey(a1) != FlowKey(inner) {
		t.Fatal("IPIP key should equal native inner key")
	}
}
