package dialer

import (
	"sync"
	"time"
)

// With get-slots > 2 the h_tunnel client runs several data GET loops that
// complete in arbitrary order. The server numbers data-bearing batches
// (HeaderBatchSeq) but the HTTP responses still arrive out of order, so
// handing frames straight to the P2P session would reorder packets of the
// same inner flow. That reordering collapses inner TCP windows (spurious
// fast retransmits) and made multi-GET throughput no better than a single
// loop. orderedInbox buffers batches and delivers them strictly by sequence.
const (
	reorderGapTimeout = time.Second
	reorderMaxBytes   = 8 << 20
	reorderTick       = 500 * time.Millisecond
)

type orderedBatch struct {
	body     []byte
	loopType string
	readSeq  int
}

type orderedInbox struct {
	mu          sync.Mutex
	next        uint64
	pending     map[uint64]orderedBatch
	bytes       int
	lastAdvance time.Time
	deliver     func(orderedBatch)
	closed      <-chan struct{}
}

func newOrderedInbox(deliver func(orderedBatch), closed <-chan struct{}) *orderedInbox {
	o := &orderedInbox{
		// The server numbers the first data batch of a channel as 1, so the
		// expected base is known from the start; no first-seen guessing (which
		// would drop earlier batches if a later one completed first).
		next:        1,
		pending:     make(map[uint64]orderedBatch),
		lastAdvance: time.Now(),
		deliver:     deliver,
		closed:      closed,
	}
	go o.ticker()
	return o
}

// push buffers one sequenced batch. Delivery happens under o.mu so batches
// are handed to the P2P session in sequence order even when several recvLoops
// call push concurrently. A gap older than reorderGapTimeout (or a buffer over
// reorderMaxBytes) is skipped: the inner TCP will retransmit what the carrier
// actually lost, instead of stalling every flow forever.
func (o *orderedInbox) push(seq uint64, b orderedBatch) {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := time.Now()
	if seq < o.next {
		return // stale duplicate
	}
	if _, dup := o.pending[seq]; dup {
		return
	}
	if seq == o.next {
		o.next++
		o.deliver(b)
		o.lastAdvance = now
	} else {
		o.pending[seq] = b
		o.bytes += len(b.body)
	}
	o.drainLocked(now)
}

func (o *orderedInbox) drainLocked(now time.Time) {
	if len(o.pending) > 0 && (now.Sub(o.lastAdvance) > reorderGapTimeout || o.bytes > reorderMaxBytes) {
		var min uint64
		first := true
		for s := range o.pending {
			if first || s < min {
				min = s
				first = false
			}
		}
		if !first && min > o.next {
			// Drop anything older than the new base (none expected, but keep
			// the byte accounting exact) and skip the missing range.
			for s, b := range o.pending {
				if s < min {
					o.bytes -= len(b.body)
					delete(o.pending, s)
				}
			}
			o.next = min
			o.lastAdvance = now
		}
	}
	for {
		b, ok := o.pending[o.next]
		if !ok {
			return
		}
		delete(o.pending, o.next)
		o.bytes -= len(b.body)
		o.next++
		o.deliver(b)
		o.lastAdvance = time.Now()
	}
}

func (o *orderedInbox) tick() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.drainLocked(time.Now())
}

func (o *orderedInbox) ticker() {
	t := time.NewTicker(reorderTick)
	defer t.Stop()
	for {
		select {
		case <-o.closed:
			return
		case <-t.C:
			o.tick()
		}
	}
}
