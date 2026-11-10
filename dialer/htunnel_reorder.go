package dialer

import (
	"sync"
	"time"

	"phaethon/util"
)

// The h_tunnel client issues GETs one after another. If the recvLoop parses and
// delivers a whole batch synchronously (routeFrame can block on the P2P
// session's inbound channel), it stops issuing GETs while the server keeps
// queueing frames. Server-side stats showed waiters=0 with 0.5~1.4MB queued and
// sojourn up to 585ms — that queue is what inflates the inner TCP RTT and
// halves relay throughput.
//
// orderedInbox decouples the two: recvLoop pushes a decrypted, sequenced batch
// and immediately returns to the next GET; a single delivery goroutine parses
// and routes batches strictly in sequence order. With one data loop the pushes
// are already ordered; the sequence map still protects the multi-GET case.
const (
	reorderGapTimeout = 5 * time.Second
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
	ready       []orderedBatch
	bytes       int // bytes held in pending (not yet ready)
	lastAdvance time.Time
	deliver     func(orderedBatch)
	closed      <-chan struct{}
	wake        chan struct{}
}

func newOrderedInbox(deliver func(orderedBatch), closed <-chan struct{}) *orderedInbox {
	o := &orderedInbox{
		// The server numbers the first data batch of a channel as 1, so the
		// expected base is known from the start.
		next:        1,
		pending:     make(map[uint64]orderedBatch),
		lastAdvance: time.Now(),
		deliver:     deliver,
		closed:      closed,
		wake:        make(chan struct{}, 1),
	}
	go o.run()
	return o
}

// push enqueues one sequenced batch and returns immediately. It never calls
// deliver, so the caller (recvLoop) can issue the next GET without waiting for
// the P2P session to drain.
func (o *orderedInbox) push(seq uint64, b orderedBatch) {
	o.mu.Lock()
	now := time.Now()
	if seq < o.next {
		o.mu.Unlock()
		return // stale duplicate
	}
	if _, dup := o.pending[seq]; dup {
		o.mu.Unlock()
		return
	}
	if seq == o.next {
		o.next++
		o.ready = append(o.ready, b)
		o.lastAdvance = now
	} else {
		o.pending[seq] = b
		o.bytes += len(b.body)
	}
	o.drainLocked(now)
	o.mu.Unlock()

	select {
	case o.wake <- struct{}{}:
	default:
	}
}

// drainLocked moves consecutively numbered batches into ready, skipping a gap
// that has persisted longer than reorderGapTimeout (or when the buffer is
// over its byte cap). Caller holds o.mu.
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
			util.LogModuleWarn("htunnel", "inbox gap skip: expected=%d resume=%d waited=%v pending=%d bytes=%d",
				o.next, min, now.Sub(o.lastAdvance), len(o.pending), o.bytes)
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
		o.ready = append(o.ready, b)
		o.lastAdvance = time.Now()
	}
}

// run is the single delivery goroutine: FIFO from ready, preserving order.
func (o *orderedInbox) run() {
	t := time.NewTicker(reorderTick)
	defer t.Stop()
	for {
		select {
		case <-o.closed:
			return
		case <-o.wake:
		case <-t.C:
		}
		o.mu.Lock()
		o.drainLocked(time.Now())
		o.mu.Unlock()
		for {
			o.mu.Lock()
			if len(o.ready) == 0 {
				o.mu.Unlock()
				break
			}
			b := o.ready[0]
			o.ready = o.ready[1:]
			o.mu.Unlock()
			o.deliver(b)
		}
	}
}
