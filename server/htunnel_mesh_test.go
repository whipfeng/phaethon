package server

import (
	"testing"
	"time"
)

func newTestChannel() *htChannel {
	ch := &htChannel{
		closed:      make(chan struct{}),
		meshIn:      make(chan meshMsg, 32),
		postNext:    1,
		postPending: make(map[uint64][]meshMsg),
		postWake:    make(chan struct{}, 1),
	}
	go ch.postDeliveryLoop()
	return ch
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
	if got := drainPost(t, ch, 1); got[0] != 1 {
		t.Fatalf("first frame = %v", got)
	}
	ch.enqueuePost(3, []meshMsg{{payload: []byte{3}}}) // seq 2 lost
	time.Sleep(300 * time.Millisecond)                // let the delivery loop settle

	// Pretend the gap has persisted beyond the timeout, run the skip
	// synchronously, then deliver seq 4.
	ch.postMu.Lock()
	ch.postGapAt = time.Now().Add(-2 * postReorderGap)
	ch.postMu.Unlock()
	_ = ch.postSkipGapIfStale()
	ch.enqueuePost(4, []meshMsg{{payload: []byte{4}}})

	got := drainPost(t, ch, 2)
	if got[0] != 3 || got[1] != 4 {
		t.Fatalf("lost-post skip delivered %v, want [3 4]", got)
	}
}
