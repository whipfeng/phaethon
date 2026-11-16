package mesh

import (
	"math"
	"testing"
)

func TestPeerQualityRetransRateDelta(t *testing.T) {
	q := NewPeerQuality()
	q.UpdateCarrierStats(100, 5)
	if _, ok := q.RetransRate(); !ok {
		t.Fatalf("RetransRate not marked available after first sample")
	}
	q.UpdateCarrierStats(200, 15) // 10 retrans / 100 segs = 10%
	if r, ok := q.RetransRate(); !ok || math.Abs(r-0.10) > 1e-9 {
		t.Fatalf("RetransRate = %v ok=%v, want 0.10", r, ok)
	}
	// Counter regression (socket replaced) must not produce a bogus rate.
	q.UpdateCarrierStats(50, 2)
	if r, _ := q.RetransRate(); math.Abs(r-0.10) > 1e-9 {
		t.Fatalf("RetransRate after counter reset = %v, want previous 0.10", r)
	}
	// Zero new segments keeps the previous rate.
	q.UpdateCarrierStats(50, 2)
	if r, _ := q.RetransRate(); math.Abs(r-0.10) > 1e-9 {
		t.Fatalf("RetransRate with no new segments = %v, want 0.10", r)
	}
}
