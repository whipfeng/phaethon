package p2p

import "testing"

func TestStopPeerByNodeSeqPreservesOtherLinks(t *testing.T) {
	manager := NewP2PManager("local", "test", nil)
	defer manager.Stop()

	failed := &Peer{ID: "failed", NodeID: "remote", Seq: 1, stopCh: make(chan struct{})}
	healthy := &Peer{ID: "healthy", NodeID: "remote", Seq: 3, stopCh: make(chan struct{})}
	manager.peers[failed.ID] = failed
	manager.peers[healthy.ID] = healthy

	manager.StopPeerByNodeSeq("remote", 1)

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
