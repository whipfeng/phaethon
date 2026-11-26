package p2p

import (
	"math"
	"testing"
	"time"
)

func approx(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-6 {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestLinkStatsDecay verifies the evidence weight decays with τ=10min while
// keeping the loss ratio intact.
func TestLinkStatsDecay(t *testing.T) {
	now := time.Now()
	st := &linkStats{lossW: 10, sampW: 20, at: now.Add(-linkQualityForgetTau)}

	lw, sw := st.decayedAt(now)
	approx(t, sw, 20/math.E)
	approx(t, lw, 10/math.E)
	approx(t, lw/sw, 0.5)

	// Mutating decay must agree with the read-only view and advance the epoch.
	st.decayLocked(now)
	approx(t, st.sampW, sw)
	approx(t, st.lossW, lw)
	if !st.at.Equal(now) {
		t.Fatalf("at = %v, want %v", st.at, now)
	}
}

// TestPingTrackerRetireFoldsSamples verifies resolved outcomes move into the
// persisted stats and the tracker detaches.
func TestPingTrackerRetireFoldsSamples(t *testing.T) {
	tr := newPingTracker()
	st := &linkStats{at: time.Now()}
	key := linkQualityKey{nodeID: "gg", linkName: "p1"}
	tr.attachStats(key, st, 0, 0, 0)

	tr.mu.Lock()
	tr.samples = []pingSample{{seq: 1, acked: true}, {seq: 2}, {seq: 3}}
	tr.mu.Unlock()

	gotKey, ok := tr.retire(time.Now())
	if !ok {
		t.Fatal("retire returned ok=false, want true")
	}
	if gotKey != key {
		t.Fatalf("key = %+v, want %+v", gotKey, key)
	}
	approx(t, st.sampW, 3)
	approx(t, st.lossW, 2)

	if tr.stats != nil || tr.samples != nil || len(tr.pendingPings) != 0 || len(tr.onWire) != 0 {
		t.Fatalf("tracker not reset: stats=%v samples=%v pending=%d onWire=%d",
			tr.stats, tr.samples, len(tr.pendingPings), len(tr.onWire))
	}

	// A second retire must not double fold.
	if _, ok := tr.retire(time.Now()); ok {
		t.Fatal("second retire returned ok=true, want false")
	}
	approx(t, st.sampW, 3)
}

// TestPingTrackerRetireCensorsUnwrittenPings verifies local admission-control
// drops never become link loss, while a written ping older than an RTO does.
func TestPingTrackerRetireCensorsUnwrittenPings(t *testing.T) {
	tr := newPingTracker()
	st := &linkStats{at: time.Now()}
	tr.attachStats(linkQualityKey{nodeID: "gg"}, st, 0, 0, 0)

	tr.recordPing() // seq 1: queued but never written
	seq := tr.recordPing()
	tr.markOnWire(seq)
	tr.mu.Lock()
	tr.onWire[seq] = time.Now().Add(-time.Second) // written, no pong for 1s
	tr.mu.Unlock()

	tr.retire(time.Now())
	approx(t, st.sampW, 1)
	approx(t, st.lossW, 1)
}

// TestPingTrackerRetireCensorsFreshPing verifies a ping written less than an
// RTO ago is censored, not counted as loss.
func TestPingTrackerRetireCensorsFreshPing(t *testing.T) {
	tr := newPingTracker()
	st := &linkStats{at: time.Now()}
	tr.attachStats(linkQualityKey{nodeID: "gg"}, st, 0, 0, 0)

	tr.recordPing()
	tr.markOnWire(1)

	tr.retire(time.Now())
	approx(t, st.sampW, 0)
	approx(t, st.lossW, 0)
}

// TestGetLossRateMergesPersistedHistory verifies a new generation inherits
// the decayed loss ratio instead of reporting 0% before its window fills.
func TestGetLossRateMergesPersistedHistory(t *testing.T) {
	tr := newPingTracker()
	tr.attachStats(linkQualityKey{nodeID: "gg"}, &linkStats{lossW: 9, sampW: 10, at: time.Now()}, 0, 0, 0)

	approx(t, tr.getLossRate(), 0.9)

	tr.mu.Lock()
	tr.samples = []pingSample{{seq: 1, acked: true}}
	tr.mu.Unlock()
	approx(t, tr.getLossRate(), 9.0/11.0)
}

// TestManagerLinkQualitySharesAcrossGenerations verifies two sessions of the
// same logical link attach to the same persisted state.
func TestManagerLinkQualitySharesAcrossGenerations(t *testing.T) {
	m := &P2PManager{}

	gen1 := &Peer{NodeID: "gg", ProxyName: "p1", pingTracker: newPingTracker()}
	m.attachLinkQuality(gen1)
	st1 := gen1.pingTracker.stats
	if st1 == nil {
		t.Fatal("gen1 not attached")
	}
	gen1.pingTracker.mu.Lock()
	gen1.pingTracker.samples = []pingSample{{seq: 1}, {seq: 2}}
	gen1.pingTracker.mu.Unlock()
	m.retireLinkQuality(gen1)

	gen2 := &Peer{NodeID: "gg", ProxyName: "p1", pingTracker: newPingTracker()}
	m.attachLinkQuality(gen2)
	if gen2.pingTracker.stats != st1 {
		t.Fatal("gen2 did not inherit gen1's link stats")
	}
	approx(t, gen2.pingTracker.getLossRate(), 1.0)
}

// TestManagerLinkQualityDistinctProxies verifies the key includes the proxy:
// the same node over two proxies keeps separate evidence.
func TestManagerLinkQualityDistinctProxies(t *testing.T) {
	m := &P2PManager{}

	a := &Peer{NodeID: "gg", ProxyName: "p1", pingTracker: newPingTracker()}
	b := &Peer{NodeID: "gg", ProxyName: "p2", pingTracker: newPingTracker()}
	m.attachLinkQuality(a)
	m.attachLinkQuality(b)
	if a.pingTracker.stats == b.pingTracker.stats {
		t.Fatal("distinct proxies share one link stats entry")
	}
}

// TestLinkQualityIdentityPassive verifies passive sessions (no local proxy)
// key on the friendly name the active side advertises, so consecutive passive
// generations of the same remote proxy share evidence.
func TestLinkQualityIdentityPassive(t *testing.T) {
	passive := &Peer{NodeID: "vm", FriendlyName: "ggP"}
	key, ok := passive.linkQualityIdentity()
	if !ok {
		t.Fatal("passive identity rejected")
	}
	if key != (linkQualityKey{nodeID: "vm", linkName: "ggP"}) {
		t.Fatalf("key = %+v, want {vm ggP}", key)
	}

	m := &P2PManager{}
	gen1 := &Peer{NodeID: "vm", FriendlyName: "ggP", pingTracker: newPingTracker()}
	m.attachLinkQuality(gen1)
	st1 := gen1.pingTracker.stats
	gen1.pingTracker.mu.Lock()
	gen1.pingTracker.samples = []pingSample{{seq: 1}}
	gen1.pingTracker.mu.Unlock()
	m.retireLinkQuality(gen1)

	gen2 := &Peer{NodeID: "vm", FriendlyName: "ggP", pingTracker: newPingTracker()}
	m.attachLinkQuality(gen2)
	if gen2.pingTracker.stats != st1 {
		t.Fatal("passive generation did not inherit the previous generation's stats")
	}

	// A different remote proxy on the same node must not share the entry.
	other := &Peer{NodeID: "vm", FriendlyName: "ggQ", pingTracker: newPingTracker()}
	m.attachLinkQuality(other)
	if other.pingTracker.stats == st1 {
		t.Fatal("distinct remote proxies share one link stats entry")
	}
}

// TestLinkStatsRTTDecay verifies only the RTT evidence weight decays; the
// means themselves stay put (a mean cannot fade toward zero).
func TestLinkStatsRTTDecay(t *testing.T) {
	now := time.Now()
	st := &linkStats{srtt: 80 * time.Millisecond, rttVar: 20 * time.Millisecond, rttW: 10, at: now.Add(-linkQualityForgetTau)}

	srtt, rttVar, w := st.rttDecayedAt(now)
	if srtt != 80*time.Millisecond || rttVar != 20*time.Millisecond {
		t.Fatalf("means decayed: srtt=%v rttVar=%v", srtt, rttVar)
	}
	approx(t, w, 10/math.E)
}

// TestRetireFoldsRTTSamples verifies a generation's RTT/jitter become the
// persisted means, weighted by the sample count.
func TestRetireFoldsRTTSamples(t *testing.T) {
	tr := newPingTracker()
	st := &linkStats{at: time.Now()}
	tr.attachStats(linkQualityKey{nodeID: "gg"}, st, 0, 0, 0)

	tr.rttSampler.add(40 * time.Millisecond)
	tr.rttSampler.add(60 * time.Millisecond)
	wantSrtt, wantVar, n := tr.rttSampler.getStats()
	if n != 2 {
		t.Fatalf("sample count = %d, want 2", n)
	}

	if _, ok := tr.retire(time.Now()); !ok {
		t.Fatal("retire returned ok=false")
	}
	approx(t, st.rttW, 2)
	if st.srtt != wantSrtt || st.rttVar != wantVar {
		t.Fatalf("folded srtt=%v rttVar=%v, want %v/%v", st.srtt, st.rttVar, wantSrtt, wantVar)
	}
}

// TestRetireRTTWeightedMerge verifies folding blends with the previous
// generation instead of overwriting it.
func TestRetireRTTWeightedMerge(t *testing.T) {
	now := time.Now()
	tr := newPingTracker()
	st := &linkStats{srtt: 100 * time.Millisecond, rttVar: 10 * time.Millisecond, rttW: 1, at: now}
	tr.attachStats(linkQualityKey{nodeID: "gg"}, st, 0, 0, 0)

	tr.rttSampler.add(200 * time.Millisecond)
	tr.retire(now)

	// (100ms*1 + 200ms*1) / 2 = 150ms
	if st.srtt != 150*time.Millisecond {
		t.Fatalf("merged srtt = %v, want 150ms", st.srtt)
	}
	approx(t, st.rttW, 2)
}

// TestAttachSeedsRTTFromHistory verifies a new generation starts from the
// previous generation's RTT/jitter rather than zero.
func TestAttachSeedsRTTFromHistory(t *testing.T) {
	m := &P2PManager{}

	gen1 := &Peer{NodeID: "gg", ProxyName: "p1", pingTracker: newPingTracker()}
	m.attachLinkQuality(gen1)
	gen1.pingTracker.rttSampler.add(120 * time.Millisecond)
	wantSrtt, wantVar, _ := gen1.pingTracker.rttSampler.getStats()
	m.retireLinkQuality(gen1)

	gen2 := &Peer{NodeID: "gg", ProxyName: "p1", pingTracker: newPingTracker()}
	m.attachLinkQuality(gen2)

	srtt, rttVar, n := gen2.pingTracker.rttSampler.getStats()
	if n != 0 {
		t.Fatalf("new generation sample count = %d, want 0", n)
	}
	if srtt != wantSrtt || rttVar != wantVar {
		t.Fatalf("seeded srtt=%v rttVar=%v, want %v/%v", srtt, rttVar, wantSrtt, wantVar)
	}
}

// TestSeedIgnoresEmptyHistory verifies no seed happens without evidence.
func TestSeedIgnoresEmptyHistory(t *testing.T) {
	r := &rttSampler{}
	r.seed(0, 0, 0)
	if srtt, _, n := r.getStats(); srtt != 0 || n != 0 {
		t.Fatalf("empty seed changed sampler: srtt=%v n=%d", srtt, n)
	}
}
