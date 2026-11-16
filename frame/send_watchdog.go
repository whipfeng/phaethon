package frame

import (
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"phaethon/util"
)

// Carrier send-stall watchdog.
//
// A carrier is a single ordered TCP byte stream (plain TCP or the WebSocket
// leg layered on it). When one segment is lost near the tail of the stream,
// TCP can recover it only through the retransmission timer; on a lossy WAN the
// exponential backoff reaches 2^6..2^7 x RTO (14-28s). Everything behind that
// hole — data, control frames, pongs — is HOL-blocked for the whole episode.
//
// The write path gives us a direct, platform-neutral signal: with
// TCP_NOTSENT_LOWAT the socket buffer is bounded, so a blocked hole eventually
// blocks the writer inside one Send/SendBatch call. The guard closes the
// carrier when a single write has been blocked longer than the limit, forcing
// the p2p layer to re-establish the session/lane with a fresh RTO instead of
// waiting out the backoff.
//
// PHAETHON_CARRIER_STALL_MS enables it (0/unset = disabled, preserving the
// historical "no write deadline" behavior). A nil guard is a no-op so call
// sites stay branch-free.
type sendStallGuard struct {
	limit   time.Duration
	closeFn func()

	start  atomic.Int64 // UnixNano of the in-flight write; 0 = idle
	fired  atomic.Bool
	done   chan struct{}
	stopMu sync.Once
}

// carrierSendStallLimit reads PHAETHON_CARRIER_STALL_MS.
func carrierSendStallLimit() time.Duration {
	v := os.Getenv("PHAETHON_CARRIER_STALL_MS")
	if v == "" {
		return 0
	}
	ms, err := strconv.Atoi(v)
	if err != nil || ms <= 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

// newSendStallGuard returns nil when the watchdog is disabled.
func newSendStallGuard(limit time.Duration, closeFn func()) *sendStallGuard {
	if limit <= 0 || closeFn == nil {
		return nil
	}
	g := &sendStallGuard{limit: limit, closeFn: closeFn, done: make(chan struct{})}
	go g.loop()
	return g
}

func (g *sendStallGuard) loop() {
	tick := g.limit / 4
	if tick < 50*time.Millisecond {
		tick = 50 * time.Millisecond
	}
	if tick > 500*time.Millisecond {
		tick = 500 * time.Millisecond
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-g.done:
			return
		case <-t.C:
			start := g.start.Load()
			if start == 0 {
				continue
			}
			blocked := time.Since(time.Unix(0, start))
			if blocked <= g.limit || !g.fired.CompareAndSwap(false, true) {
				continue
			}
			util.LogModuleWarn("p2p", "[CARRIER-STALL] carrier write blocked %v > limit %v; resetting carrier to escape RTO backoff",
				blocked.Round(time.Millisecond), g.limit)
			g.closeFn()
			return
		}
	}
}

func (g *sendStallGuard) begin() {
	if g == nil {
		return
	}
	g.start.Store(time.Now().UnixNano())
}

func (g *sendStallGuard) end() {
	if g == nil {
		return
	}
	g.start.Store(0)
	g.fired.Store(false)
}

func (g *sendStallGuard) stop() {
	if g == nil {
		return
	}
	g.stopMu.Do(func() { close(g.done) })
}
