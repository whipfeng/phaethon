package mesh

import (
	"net"
	"testing"
)

// A LinkNIC whose session ended must never be handed out by route selection:
// the async cleanup can leave it in the route table and topology for a window,
// and writing into it black-holes packets until the next recompute.
func TestNextHopLinkSkipsClosedLink(t *testing.T) {
	m := newTestMeshManager(t, "self", "100.64.1.0/24", nil)
	_, subnet, err := net.ParseCIDR("100.64.2.0/24")
	if err != nil {
		t.Fatal(err)
	}
	nic := NewLinkNIC(7, &hysteresisPeerSender{nodeID: "peerA"}, "peerA", subnet)
	m.routeTable.Store(&routeTable{bestPaths: map[string]dijkstraResult{
		"peerA": {NextHop: "peerA", LinkNIC: nic, Cost: 10},
		"peerB": {NextHop: "peerA", LinkNIC: nic, Cost: 20},
	}})

	if hop, got := m.NextHopLink("peerB"); hop != "peerA" || got != nic {
		t.Fatalf("live link: NextHopLink = (%q, %p), want (peerA, %p)", hop, got, nic)
	}

	nic.MarkClosed()
	if hop, got := m.NextHopLink("peerB"); hop != "" || got != nil {
		t.Fatalf("closed link handed out: NextHopLink = (%q, %p), want (, nil)", hop, got)
	}
}

// After a reconnect the target is a direct peer again with a fresh LinkNIC.
// A route-table path still pointing at the closed generation must fall through
// to the live direct link instead of reporting no route.
func TestNextHopLinkFallsBackToFreshDirectLink(t *testing.T) {
	m := newTestMeshManager(t, "self", "100.64.1.0/24", nil)
	_, subnet, err := net.ParseCIDR("100.64.2.0/24")
	if err != nil {
		t.Fatal(err)
	}
	oldNIC := NewLinkNIC(7, &hysteresisPeerSender{nodeID: "peerA"}, "peerA", subnet)
	oldNIC.MarkClosed()
	newNIC := NewLinkNIC(8, &hysteresisPeerSender{nodeID: "peerA"}, "peerA", subnet)
	m.topology.RegisterPeer(&hysteresisPeerSender{nodeID: "peerA", nic: newNIC})
	m.routeTable.Store(&routeTable{bestPaths: map[string]dijkstraResult{
		"peerA": {NextHop: "peerA", LinkNIC: oldNIC, Cost: 10},
	}})

	hop, got := m.NextHopLink("peerA")
	if hop != "peerA" || got != newNIC {
		t.Fatalf("NextHopLink = (%q, %p), want (peerA, %p)", hop, got, newNIC)
	}
}
