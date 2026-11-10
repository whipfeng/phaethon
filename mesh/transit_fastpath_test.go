package mesh

import (
	"net"
	"testing"
)

func fastPathFrame(dst net.IP) []byte {
	f := make([]byte, 20)
	f[0] = 0x45 // IPv4, IHL=5
	f[8] = 64   // TTL
	f[9] = 6    // TCP
	copy(f[12:16], net.IPv4(100, 212, 0, 1).To4())
	copy(f[16:20], dst.To4())
	return f
}

func newFastPathMesh(sender PeerSender) *MeshManager {
	_, subnet, _ := net.ParseCIDR("100.0.0.0/16")
	m := &MeshManager{nodeID: "qg", subnet: subnet}
	m.routeTable.Store(&routeTable{
		bestPaths: map[string]dijkstraResult{
			"jf": {NextHop: "jf"},
			"vm": {NextHop: "vm"},
		},
		subnetOwner: map[[4]byte]string{
			{100, 0, 0, 0}:   "qg",
			{100, 2, 0, 0}:   "jf",
			{100, 212, 0, 0}: "vm",
		},
		nextHopSenders: map[string]PeerSender{"jf": sender, "vm": sender},
	})
	return m
}

func TestTransitFastPathForwards(t *testing.T) {
	sender := &fakePeerSender{}
	m := newFastPathMesh(sender)
	frame := fastPathFrame(net.IPv4(100, 2, 0, 10))
	released := 0

	if !m.tryTransitFastPath("vm", net.IPv4(100, 2, 0, 10), frame, func() { released++ }) {
		t.Fatal("transit frame was not fast-pathed")
	}
	if len(sender.sent) != 1 {
		t.Fatalf("sent %d frames, want 1", len(sender.sent))
	}
	out := sender.sent[0]
	if out[8] != 63 {
		t.Fatalf("TTL = %d, want 63", out[8])
	}
	if ipv4HeaderChecksum(out[:20]) != 0 {
		t.Fatalf("header checksum invalid after rewrite")
	}
	if released != 1 {
		t.Fatalf("release called %d times, want 1", released)
	}
	if frame[8] != 64 {
		t.Fatalf("original frame mutated: TTL=%d", frame[8])
	}
}

func TestTransitFastPathFallsBack(t *testing.T) {
	sender := &fakePeerSender{}
	m := newFastPathMesh(sender)

	cases := []struct {
		name   string
		from   string
		dst    net.IP
		mutate func([]byte)
	}{
		{"local destination", "vm", net.IPv4(100, 0, 0, 5), nil},
		{"unknown destination", "vm", net.IPv4(10, 1, 2, 3), nil},
		{"next hop is sender", "jf", net.IPv4(100, 2, 0, 10), nil},
		{"ttl exhausted", "vm", net.IPv4(100, 2, 0, 10), func(f []byte) { f[8] = 1 }},
	}
	for _, tc := range cases {
		if tc.name == "local destination" {
			// dst lives in our own /16 (owner==self) -> local delivery
			tc.dst = net.IPv4(100, 0, 0, 5)
		}
		frame := fastPathFrame(tc.dst)
		if tc.mutate != nil {
			tc.mutate(frame)
		}
		released := 0
		if m.tryTransitFastPath(tc.from, tc.dst, frame, func() { released++ }) {
			t.Fatalf("%s: unexpectedly fast-pathed", tc.name)
		}
		if released != 0 {
			t.Fatalf("%s: release called on fallback", tc.name)
		}
	}
	if len(sender.sent) != 0 {
		t.Fatalf("fallback cases sent %d frames", len(sender.sent))
	}
}
