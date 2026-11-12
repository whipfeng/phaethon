package frame

import (
	"sync"
	"time"
)

// PacedTransport is a transport-agnostic shaper: it delays data batches so the
// average send rate stays at the configured bytes/s. Control frames are never
// paced. The rate can be updated at runtime (e.g. from the peer's measured
// delivery rate).
//
// EXPERIMENTAL / NEGATIVE RESULT (2026-11-12): enabled via
// PHAETHON_PACE_STREAM=1 on the passive side it broke the link (all inner
// streams failed, one 938s hang) because the synchronous sleep holds the peer
// write loop and starves control/ACK frames; with no rate feedback yet
// (rate=0.9*0) it also throttled data to zero. Kept for the record, default
// off. A viable pacer needs an asynchronous queue + shaper goroutine with
// control priority and a rate floor, not this synchronous form.
type PacedTransport struct {
	base FrameTransport

	mu     sync.Mutex
	rate   float64
	tokens float64
	burst  float64
	last   time.Time
}

func NewPacedTransport(base FrameTransport, rate float64) *PacedTransport {
	return &PacedTransport{base: base, rate: rate, burst: 32 << 10}
}

// SetRate updates the target bytes/s. rate <= 0 disables pacing.
func (t *PacedTransport) SetRate(rate float64) {
	t.mu.Lock()
	t.rate = rate
	t.mu.Unlock()
}

func (t *PacedTransport) pace(n int) {
	if n <= 0 {
		return
	}
	t.mu.Lock()
	if t.rate <= 0 {
		t.mu.Unlock()
		return
	}
	now := time.Now()
	if t.last.IsZero() {
		t.last = now
		t.tokens = t.burst
	}
	t.tokens += now.Sub(t.last).Seconds() * t.rate
	if t.tokens > t.burst {
		t.tokens = t.burst
	}
	t.last = now
	var sleep time.Duration
	if t.tokens < float64(n) {
		sleep = time.Duration((float64(n) - t.tokens) / t.rate * float64(time.Second))
		t.tokens = 0
		t.last = now.Add(sleep)
	} else {
		t.tokens -= float64(n)
	}
	t.mu.Unlock()
	if sleep > 0 {
		time.Sleep(sleep)
	}
}

func (t *PacedTransport) Send(frameType byte, payload []byte, isControl bool) error {
	if !isControl && frameType == FrameMeshPacket {
		t.pace(len(payload) + 3)
	}
	return t.base.Send(frameType, payload, isControl)
}

func (t *PacedTransport) SendBatch(frames []Frame) error {
	n := 0
	for _, f := range frames {
		if f.Type == FrameMeshPacket {
			n += 3 + len(f.Payload)
		}
	}
	t.pace(n)
	return t.base.SendBatch(frames)
}

func (t *PacedTransport) Recv() (byte, []byte, error) { return t.base.Recv() }

func (t *PacedTransport) Close() error { return t.base.Close() }

// IsStreamCarrier delegates the capability of the wrapped transport.
func (t *PacedTransport) IsStreamCarrier() bool {
	if sc, ok := t.base.(StreamCarrier); ok {
		return sc.IsStreamCarrier()
	}
	return false
}
