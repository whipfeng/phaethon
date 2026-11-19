package p2p

import (
	"sync"
	"sync/atomic"
	"time"
)

// Lane-level async pacing. The pacer lives in each lane's writer goroutine, so
// a wait only delays that lane's data; it never blocks peerWriteLoop, control
// frames or ACKs. This is deliberately different from the old synchronous
// frame.PacedTransport, which slept on the shared write path and wedged the
// session (see docs/issues/mesh-transfer-throughput-20261109.md §110).

const (
	lanePaceBurst = 16 << 10 // bucket depth; a larger batch may borrow once
	lanePaceFloor = 512 << 10
	lanePaceCeil  = 8 << 20
	lanePaceGain  = 0.95 // leave headroom under the measured delivery rate
)

// lanePacing is the lab runtime switch. It is off by default; enabling it is a
// hidden admin action used for A/B, never a user-facing setting.
var lanePacing atomic.Bool

func SetLanePacingEnabled(on bool) { lanePacing.Store(on) }
func LanePacingEnabled() bool      { return lanePacing.Load() }

type lanePacer struct {
	mu     sync.Mutex
	rate   float64 // bytes/s; 0 = disabled/unknown
	tokens float64
	last   time.Time
}

func newLanePacer() *lanePacer { return &lanePacer{} }

// setRate clamps a measured delivery rate into the pacing band. A zero or
// negative sample disables pacing until a real rate is observed; it must never
// become 0.95×0 and stop the lane.
func (p *lanePacer) setRate(rate float64) {
	if rate <= 0 {
		p.mu.Lock()
		p.rate = 0
		p.tokens = 0
		p.last = time.Time{}
		p.mu.Unlock()
		return
	}
	r := rate * lanePaceGain
	if r < lanePaceFloor {
		r = lanePaceFloor
	}
	if r > lanePaceCeil {
		r = lanePaceCeil
	}
	p.mu.Lock()
	p.rate = r
	p.mu.Unlock()
}

// wait blocks until n data bytes may be sent at the current rate. It returns
// false when the lane/session is closed. Control frames must not call this.
func (p *lanePacer) wait(n int, laneDone, sessionClosed <-chan struct{}) bool {
	if n <= 0 {
		return true
	}
	p.mu.Lock()
	rate := p.rate
	if rate <= 0 {
		p.mu.Unlock()
		return true
	}
	now := time.Now()
	if p.last.IsZero() {
		p.last = now
	}
	elapsed := now.Sub(p.last).Seconds()
	if elapsed > 0 {
		p.tokens += rate * elapsed
	}
	burst := float64(lanePaceBurst)
	if float64(n) > burst {
		burst = float64(n) // one oversized batch may borrow
	}
	if p.tokens > burst {
		p.tokens = burst
	}
	p.last = now
	if p.tokens >= float64(n) {
		p.tokens -= float64(n)
		p.mu.Unlock()
		return true
	}
	need := float64(n) - p.tokens
	p.tokens = 0
	p.mu.Unlock()

	delay := time.Duration(need / rate * float64(time.Second))
	if delay <= 0 {
		return true
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-laneDone:
		return false
	case <-sessionClosed:
		return false
	}
}
