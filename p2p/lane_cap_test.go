package p2p

import (
	"testing"
	"time"
)

// newCapTestFlow builds a gated lane on a healthy 8ms path with inflight far
// below cap/2 but with acked bytes advancing since the last adjust.
func newCapTestFlow() *laneFlow {
	f := &laneFlow{gated: true, capVal: laneCapInitial}
	f.minBuckets[0] = 8 * time.Millisecond
	f.srtt = 8 * time.Millisecond
	f.tx.Store(300 << 10)
	f.acked = 280 << 10
	f.lastAdjustAcked = 100 << 10
	f.lastAdjustAt = time.Now().Add(-time.Second)
	return f
}

// TestLaneCapGrowsWithAckedDemand verifies the fixed growth rule: acked
// progress is enough to grow, even when instantaneous inflight < cap/2.
func TestLaneCapGrowsWithAckedDemand(t *testing.T) {
	f := newCapTestFlow()
	f.adjustCap(time.Now())
	want := int64(laneCapInitial + laneCapInitial/4)
	if f.capVal != want {
		t.Fatalf("cap = %d, want %d", f.capVal, want)
	}
}

// TestLaneCapLegacyGrowthNeedsInflight pins the old rule behind the env flag
// so the A/B baseline stays reproducible.
func TestLaneCapLegacyGrowthNeedsInflight(t *testing.T) {
	t.Setenv("PHAETHON_LANE_CAP_LEGACY", "1")
	f := newCapTestFlow()
	f.adjustCap(time.Now())
	if f.capVal != laneCapInitial {
		t.Fatalf("legacy cap = %d, want %d (no growth with inflight < cap/2)", f.capVal, laneCapInitial)
	}
}

// TestLaneCapShrinksOnQueueDelay verifies the congestion branch still wins
// over growth when queueing delay exceeds the target.
func TestLaneCapShrinksOnQueueDelay(t *testing.T) {
	f := newCapTestFlow()
	f.minBuckets[0] = 8 * time.Millisecond
	f.srtt = 400 * time.Millisecond // qd = 392ms > 200ms target
	f.adjustCap(time.Now())
	if f.capVal >= laneCapInitial {
		t.Fatalf("cap = %d, want < %d", f.capVal, laneCapInitial)
	}
	if f.capVal < laneCapMin {
		t.Fatalf("cap = %d, below laneCapMin %d", f.capVal, laneCapMin)
	}
}
