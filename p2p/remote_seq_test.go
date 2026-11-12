package p2p

import "testing"

// TestUpdateRemoteSeq verifies stale remote link seqs are corrected from
// subsequent gossip, while zero (legacy) values are ignored.
func TestUpdateRemoteSeq(t *testing.T) {
	p := &Peer{RemoteSeq: 6}
	if !updateRemoteSeq(p, 7) {
		t.Fatal("expected correction 6 -> 7")
	}
	if p.RemoteSeq != 7 {
		t.Fatalf("RemoteSeq = %d, want 7", p.RemoteSeq)
	}
	if updateRemoteSeq(p, 7) {
		t.Fatal("same seq must not report a change")
	}
	if updateRemoteSeq(p, 0) {
		t.Fatal("zero seq must be ignored")
	}
	if p.RemoteSeq != 7 {
		t.Fatalf("RemoteSeq = %d, want 7 after ignored updates", p.RemoteSeq)
	}
}
