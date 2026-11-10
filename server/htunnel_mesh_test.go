package server

import (
	"testing"
	"time"
)

func newTestChannel() *htChannel {
	return &htChannel{
		closed:      make(chan struct{}),
		meshIn:      make(chan meshMsg, 32),
		postNext:    1,
		postPending: make(map[uint64][]meshMsg),
	}
}

func drainPost(t *testing.T, ch *htChannel, want int) []byte {
	t.Helper()
	got := make([]byte, 0, want)
	for i := 0; i < want; i++ {
		select {
		case m := <-ch.meshIn:
			got = append(got, m.payload...)
		case <-time.After(time.Second):
			t.Fatalf("timed out after %d/%d frames: %v", i, want, got)
		}
	}
	return got
}

func TestEnqueuePostOrdersConcurrentPosts(t *testing.T) {
	ch := newTestChannel()
	// Concurrent POSTs complete out of order.
	for _, seq := range []uint64{3, 1, 4, 2} {
		ch.enqueuePost(seq, []meshMsg{{payload: []byte{byte(seq)}}})
	}
	got := drainPost(t, ch, 4)
	for i, v := range got {
		if v != byte(i+1) {
			t.Fatalf("order broken: %v", got)
		}
	}
}

func TestEnqueuePostHeartbeatFillsGap(t *testing.T) {
	ch := newTestChannel()
	ch.enqueuePost(1, []meshMsg{{payload: []byte{1}}})
	ch.enqueuePost(3, []meshMsg{{payload: []byte{3}}}) // seq 2 is a heartbeat
	ch.enqueuePost(2, nil)                            // heartbeat consumed seq 2
	got := drainPost(t, ch, 2)
	if len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Fatalf("heartbeat gap handling delivered %v, want [1 3]", got)
	}
}

func TestEnqueuePostSkipsLostPost(t *testing.T) {
	ch := newTestChannel()
	ch.enqueuePost(1, []meshMsg{{payload: []byte{1}}})
	ch.enqueuePost(3, []meshMsg{{payload: []byte{3}}}) // seq 2 lost
	// Pretend the gap has persisted beyond the timeout, then deliver seq 4.
	ch.postMu.Lock()
	ch.postGapAt = time.Now().Add(-2 * postReorderGap)
	ch.postMu.Unlock()
	ch.enqueuePost(4, []meshMsg{{payload: []byte{4}}})
	got := drainPost(t, ch, 2)
	if len(got) != 2 || got[0] != 1 || got[1] != 3 {
		// seq 4 may have been delivered too; only the first two matter here.
		if len(got) < 2 || got[0] != 1 || got[1] != 3 {
			t.Fatalf("lost-post skip delivered %v, want prefix [1 3]", got)
		}
	}
}
