package p2p

import "testing"

func TestStopPeerByNodeIDStopsAllMatchingSessions(t *testing.T) {
	manager := NewP2PManager("local", "test", nil)
	defer manager.Stop()

	first := &Peer{ID: "first", NodeID: "remote", stopCh: make(chan struct{})}
	second := &Peer{ID: "second", NodeID: "remote", stopCh: make(chan struct{})}
	other := &Peer{ID: "other", NodeID: "other", stopCh: make(chan struct{})}
	manager.peers[first.ID] = first
	manager.peers[second.ID] = second
	manager.peers[other.ID] = other

	manager.StopPeerByNodeID("remote")

	assertPeerStopped(t, first)
	assertPeerStopped(t, second)
	assertPeerRunning(t, other)
}

func TestStopPeerByLinkIDPreservesOtherLinks(t *testing.T) {
	manager := NewP2PManager("local", "test", nil)
	defer manager.Stop()

	failed := &Peer{ID: "failed", NodeID: "remote", LinkID: "1-2", stopCh: make(chan struct{})}
	healthy := &Peer{ID: "healthy", NodeID: "remote", LinkID: "3-4", stopCh: make(chan struct{})}
	manager.peers[failed.ID] = failed
	manager.peers[healthy.ID] = healthy

	manager.StopPeerByLinkID("1-2")

	assertPeerStopped(t, failed)
	assertPeerRunning(t, healthy)
}

func TestManagerStopIsIdempotentAndRejectsSessions(t *testing.T) {
	manager := NewP2PManager("local", "test", nil)
	manager.Stop()
	manager.Stop()

	if manager.beginSession() {
		t.Fatal("stopped manager accepted a new session")
	}
}

func assertPeerStopped(t *testing.T, peer *Peer) {
	t.Helper()
	select {
	case <-peer.stopCh:
	default:
		t.Fatalf("peer %s was not stopped", peer.ID)
	}
}

func assertPeerRunning(t *testing.T, peer *Peer) {
	t.Helper()
	select {
	case <-peer.stopCh:
		t.Fatalf("peer %s was unexpectedly stopped", peer.ID)
	default:
	}
}
