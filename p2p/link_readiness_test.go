package p2p

import (
	"net"
	"testing"

	"phaethon/mesh"
)

// recordingMeshHandler captures the order of LinkNIC creation vs topology
// registration. Topology/Dijkstra read sender.GetLinkNIC(), so the link must
// exist before the peer becomes visible to route selection.
type recordingMeshHandler struct {
	nic            *mesh.LinkNIC
	createCalled   bool
	registerCalled bool
	linkAtRegister *mesh.LinkNIC
}

func (h *recordingMeshHandler) HandleMeshFrame(string, *mesh.LinkNIC, []byte) {}
func (h *recordingMeshHandler) HandleTopologyGossip(mesh.PeerSender, mesh.GossipInfo) {
}
func (h *recordingMeshHandler) RegisterPeer(sender mesh.PeerSender) {
	h.registerCalled = true
	h.linkAtRegister = sender.GetLinkNIC()
}
func (h *recordingMeshHandler) UnregisterPeer(mesh.PeerSender)    {}
func (h *recordingMeshHandler) BuildGossipInfo() *mesh.GossipInfo { return nil }
func (h *recordingMeshHandler) TriggerGossip()                    {}
func (h *recordingMeshHandler) DeleteLinkNIC(*mesh.LinkNIC)       {}
func (h *recordingMeshHandler) CreateLinkNIC(_ mesh.PeerSender, _ string, _ string) *mesh.LinkNIC {
	h.createCalled = true
	return h.nic
}

func gossipWithSubnet(nodeID, subnet string) mesh.GossipInfo {
	return mesh.GossipInfo{ClaimedSubnets: []mesh.GossipClaimedSubnet{
		{NodeID: nodeID, Subnet: subnet, Hop: 0},
	}}
}

func TestRegisterPeerWithMeshPublishesLinkBeforeTopology(t *testing.T) {
	_, subnet, err := net.ParseCIDR("100.64.2.0/24")
	if err != nil {
		t.Fatal(err)
	}
	h := &recordingMeshHandler{nic: mesh.NewLinkNIC(9, nil, "peerA", subnet)}
	m := &P2PManager{meshHandler: h, linkQuality: map[linkQualityKey]*linkStats{}}
	peer := &Peer{ID: "1", pingTracker: newPingTracker()}

	if !m.registerPeerWithMesh(peer, "peerA", gossipWithSubnet("peerA", "100.64.2.0/24")) {
		t.Fatal("registerPeerWithMesh = false, want true")
	}
	if !h.createCalled || !h.registerCalled {
		t.Fatalf("createCalled=%v registerCalled=%v, want both true", h.createCalled, h.registerCalled)
	}
	if h.linkAtRegister != h.nic {
		t.Fatal("peer was registered with topology before its LinkNIC was set")
	}
	if peer.GetLinkNIC() != h.nic {
		t.Fatal("peer did not retain the created LinkNIC")
	}
}

func TestRegisterPeerWithMeshFailsWithoutLinkNIC(t *testing.T) {
	h := &recordingMeshHandler{}
	m := &P2PManager{meshHandler: h, linkQuality: map[linkQualityKey]*linkStats{}}
	peer := &Peer{ID: "1", pingTracker: newPingTracker()}

	if m.registerPeerWithMesh(peer, "peerA", gossipWithSubnet("peerA", "100.64.2.0/24")) {
		t.Fatal("registerPeerWithMesh = true, want false when LinkNIC creation fails")
	}
	if h.registerCalled {
		t.Fatal("peer was published to topology without a LinkNIC")
	}
	if peer.GetLinkNIC() != nil {
		t.Fatal("peer holds a nil LinkNIC but registers as ready")
	}
}

func TestLookupProxyPeerRouteRejectsClosedLink(t *testing.T) {
	prev := GlobalP2PManager
	defer func() { GlobalP2PManager = prev }()

	_, subnet, err := net.ParseCIDR("100.64.2.0/24")
	if err != nil {
		t.Fatal(err)
	}
	nic := mesh.NewLinkNIC(11, nil, "peerA", subnet)
	peer := &Peer{ID: "1", ProxyName: "P"}
	peer.SetLinkNIC(nic)
	GlobalP2PManager = &P2PManager{proxyPeers: map[string]*Peer{"P": peer}}

	if _, _, ok := LookupProxyPeerRoute("P"); !ok {
		t.Fatal("live link should resolve")
	}
	nic.MarkClosed()
	if _, _, ok := LookupProxyPeerRoute("P"); ok {
		t.Fatal("closed link must not resolve")
	}
}
