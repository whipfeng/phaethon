package dialer

import (
	"sync"
	"testing"
	"time"
)

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

	// Push out of order from several goroutines.
	var wg sync.WaitGroup
	order := []uint64{3, 1, 5, 2, 4, 6, 7, 9, 8, 10}
	for _, seq := range order {
		wg.Add(1)
		go func(s uint64) {
			defer wg.Done()
			inbox.push(s, orderedBatch{body: []byte{byte(s)}})
		}(seq)
	}
	wg.Wait()
	inbox.tick()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 10 {
		t.Fatalf("delivered %d batches, want 10: %v", len(got), got)
	}
	for i, v := range got {
		if v != byte(i+1) {
			t.Fatalf("delivery order broken at %d: %v", i, got)
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
	inbox.push(3, orderedBatch{body: []byte{3}}) // seq 2 is lost

	// Pretend the gap has been waiting too long, then let the ticker drain.
	inbox.mu.Lock()
	inbox.lastAdvance = time.Now().Add(-2 * reorderGapTimeout)
	inbox.mu.Unlock()
	inbox.tick()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Fatalf("gap skip delivered %v, want [1 3]", got)
	}
}
