package dialer

import (
	"sync"
	"testing"
	"time"
)

func waitFor(t *testing.T, want int, got func() []byte) []byte {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		v := got()
		if len(v) >= want {
			return v
		}
		time.Sleep(5 * time.Millisecond)
	}
	return got()
}

func TestOrderedInboxDeliversInSequence(t *testing.T) {
	closed := make(chan struct{})
	defer close(closed)

	var mu sync.Mutex
	var got []byte
	inbox := newOrderedInbox(func(b orderedBatch) {
		mu.Lock()
		got = append(got, b.body...)
		mu.Unlock()
	}, closed)

	order := []uint64{3, 1, 5, 2, 4, 6, 7, 9, 8, 10}
	var wg sync.WaitGroup
	for _, seq := range order {
		wg.Add(1)
		go func(s uint64) {
			defer wg.Done()
			inbox.push(s, orderedBatch{body: []byte{byte(s)}})
		}(seq)
	}
	wg.Wait()

	v := waitFor(t, 10, func() []byte {
		mu.Lock()
		defer mu.Unlock()
		return append([]byte(nil), got...)
	})
	if len(v) != 10 {
		t.Fatalf("delivered %d batches, want 10: %v", len(v), v)
	}
	for i, b := range v {
		if b != byte(i+1) {
			t.Fatalf("delivery order broken at %d: %v", i, v)
		}
	}
}

func TestOrderedInboxSkipsStaleGap(t *testing.T) {
	closed := make(chan struct{})
	defer close(closed)

	var mu sync.Mutex
	var got []byte
	inbox := newOrderedInbox(func(b orderedBatch) {
		mu.Lock()
		got = append(got, b.body...)
		mu.Unlock()
	}, closed)

	inbox.push(1, orderedBatch{body: []byte{1}})
	waitFor(t, 1, func() []byte { mu.Lock(); defer mu.Unlock(); return append([]byte(nil), got...) })

	inbox.push(3, orderedBatch{body: []byte{3}}) // seq 2 is lost
	time.Sleep(100 * time.Millisecond)
	inbox.mu.Lock()
	inbox.lastAdvance = time.Now().Add(-2 * reorderGapTimeout)
	inbox.mu.Unlock()

	v := waitFor(t, 2, func() []byte {
		mu.Lock()
		defer mu.Unlock()
		return append([]byte(nil), got...)
	})
	if len(v) < 2 || v[0] != 1 || v[1] != 3 {
		t.Fatalf("gap skip delivered %v, want prefix [1 3]", v)
	}
}
