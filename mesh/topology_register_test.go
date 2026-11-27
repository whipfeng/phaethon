package mesh

import "testing"

// linkTestSender is a PeerSender whose link identity is configurable, so a test
// can model several links to the same node (different proxies) and a reconnect
// of one of them (same proxy, new seq).
type linkTestSender struct {
	nodeID   string
	proxy    string
	localSeq uint16
	remoteQ  uint16
}

func (s *linkTestSender) Send([]byte) error         { return nil }
func (s *linkTestSender) SendGossip([]byte)         {}
func (s *linkTestSender) GossipFields() (bool, int) { return false, 0 }
func (s *linkTestSender) LaneAckFields() (bool, bool, bool, []GossipLaneRx, int) {
	return false, false, false, nil, 0
}
func (s *linkTestSender) GetNodeID() string       { return s.nodeID }
func (s *linkTestSender) GetProxyName() string    { return s.proxy }
func (s *linkTestSender) GetLocalSeq() uint16     { return s.localSeq }
func (s *linkTestSender) GetRemoteSeq() uint16    { return s.remoteQ }
func (s *linkTestSender) GetFriendlyName() string { return s.proxy }
func (s *linkTestSender) HasProxy() bool          { return s.proxy != "" }
func (s *linkTestSender) GetLinkNIC() *LinkNIC    { return nil }

// TestRegisterPeerKeepsMultipleLinksToSameNode reproduces the deskll case: two
// configured proxies to the same peer (DESK_TJ and DESK_TJ_WS) must both stay
// registered, otherwise the topology edge collapses to a single link.
func TestRegisterPeerKeepsMultipleLinksToSameNode(t *testing.T) {
	topo := NewTopology()
	a := &linkTestSender{nodeID: "jf", proxy: "DESK_TJ", localSeq: 11, remoteQ: 21}
	b := &linkTestSender{nodeID: "jf", proxy: "DESK_TJ_WS", localSeq: 12, remoteQ: 22}

	topo.RegisterPeer(a)
	topo.RegisterPeer(b)

	if got := len(topo.GetAllPeers()); got != 2 {
		t.Fatalf("peers after two links to the same node = %d, want 2", got)
	}
}

// TestRegisterPeerReplacesReconnectedLink: re-registering the same proxy with a
// new seq is a reconnect of one link, not a second link.
func TestRegisterPeerReplacesReconnectedLink(t *testing.T) {
	topo := NewTopology()
	old := &linkTestSender{nodeID: "jf", proxy: "DESK_TJ", localSeq: 11, remoteQ: 21}
	other := &linkTestSender{nodeID: "jf", proxy: "DESK_TJ_WS", localSeq: 12, remoteQ: 22}
	recon := &linkTestSender{nodeID: "jf", proxy: "DESK_TJ", localSeq: 31, remoteQ: 41}

	topo.RegisterPeer(old)
	topo.RegisterPeer(other)
	topo.RegisterPeer(recon)

	peers := topo.GetAllPeers()
	if len(peers) != 2 {
		t.Fatalf("peers after reconnect = %d, want 2", len(peers))
	}
	var haveOld, haveRecon, haveOther bool
	for _, p := range peers {
		switch p.Sender {
		case old:
			haveOld = true
		case recon:
			haveRecon = true
		case other:
			haveOther = true
		}
	}
	if haveOld {
		t.Fatalf("stale entry of the reconnected link was kept")
	}
	if !haveRecon || !haveOther {
		t.Fatalf("live links missing after reconnect: recon=%v other=%v", haveRecon, haveOther)
	}
}

// TestRegisterPeerKeepsDistinctInboundLinks: links without a proxy name are
// identified by their seq pair, so two inbound links from the same node stay.
func TestRegisterPeerKeepsDistinctInboundLinks(t *testing.T) {
	topo := NewTopology()
	a := &linkTestSender{nodeID: "deskll", localSeq: 5, remoteQ: 7}
	b := &linkTestSender{nodeID: "deskll", localSeq: 6, remoteQ: 8}

	topo.RegisterPeer(a)
	topo.RegisterPeer(b)

	if got := len(topo.GetAllPeers()); got != 2 {
		t.Fatalf("peers after two inbound links = %d, want 2", got)
	}
}

// TestRegisterPeerKeepsDifferentNodes guards against over-eager dedupe.
func TestRegisterPeerKeepsDifferentNodes(t *testing.T) {
	topo := NewTopology()
	topo.RegisterPeer(&linkTestSender{nodeID: "jf", proxy: "DESK_TJ", localSeq: 1, remoteQ: 2})
	topo.RegisterPeer(&linkTestSender{nodeID: "gg", proxy: "DESK_TJ", localSeq: 1, remoteQ: 2})

	if got := len(topo.GetAllPeers()); got != 2 {
		t.Fatalf("peers for two nodes = %d, want 2", got)
	}
}
