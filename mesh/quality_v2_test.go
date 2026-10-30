package mesh

import (
	"math"
	"testing"
	"time"
)

// TestNewPeerQualityPrior verifies A1: a fresh tracker carries the unprobed
// prior (100ms, loss 0) as a virtual first sample, so cost math never sees
// a zero sentinel.
func TestNewPeerQualityPrior(t *testing.T) {
	q := NewPeerQuality()
	srtt, loss, jitter := q.Stats()
	if srtt != defaultAckRTT {
		t.Fatalf("srtt = %v, want %v", srtt, defaultAckRTT)
	}
	if loss != 0 || jitter != 0 {
		t.Fatalf("loss=%v jitter=%v, want 0/0", loss, jitter)
	}
	if _, _, _, conf := q.StatsWithConfidence(); math.Abs(conf-0.1) > 1e-9 {
		t.Fatalf("confidence = %v, want 0.1 (1 sample / 10)", conf)
	}
	if q.IsStale(time.Minute) {
		t.Fatal("fresh tracker reported stale")
	}
}

// TestConfidenceDecayCurve verifies A2: confidence = min(1, n/10) · exp(-age minutes).
func TestConfidenceDecayCurve(t *testing.T) {
	q := NewPeerQuality()

	// Fresh: sampleFactor=0.1, timeFactor≈1 → ~0.1
	if _, _, _, conf := q.StatsWithConfidence(); conf > 0.11 {
		t.Fatalf("fresh confidence = %v, want ~0.1", conf)
	}

	// 10+ samples → sampleFactor saturates at 1
	q.UpdateACKStats(50*time.Millisecond, 0, 0, 10)
	if _, _, _, conf := q.StatsWithConfidence(); conf > 1.0 || conf < 0.99 {
		t.Fatalf("confidence with 10 fresh samples = %v, want ~1.0", conf)
	}

	// Age the data 1 minute → timeFactor = e^-1 ≈ 0.368
	q.mu.Lock()
	q.ackUpdated = time.Now().Add(-time.Minute)
	q.mu.Unlock()
	_, _, _, conf := q.StatsWithConfidence()
	want := math.Exp(-1)
	if math.Abs(conf-want) > 0.02 {
		t.Fatalf("confidence after 1min age = %v, want ~%v", conf, want)
	}

	// 5 minutes → e^-5 ≈ 0.0067
	q.mu.Lock()
	q.ackUpdated = time.Now().Add(-5 * time.Minute)
	q.mu.Unlock()
	if _, _, _, conf = q.StatsWithConfidence(); conf > 0.02 {
		t.Fatalf("confidence after 5min age = %v, want <0.02", conf)
	}
}

// TestResetRestoresPrior verifies Reset returns to the A1 prior state.
func TestResetRestoresPrior(t *testing.T) {
	q := NewPeerQuality()
	q.UpdateACKStats(10*time.Millisecond, 0.5, 3*time.Millisecond, 99)
	q.Reset()
	srtt, loss, _ := q.Stats()
	if srtt != defaultAckRTT || loss != 0 {
		t.Fatalf("after Reset srtt=%v loss=%v, want prior %v/0", srtt, loss, defaultAckRTT)
	}
	if _, _, _, conf := q.StatsWithConfidence(); math.Abs(conf-0.1) > 0.01 {
		t.Fatalf("after Reset confidence = %v, want ~0.1", conf)
	}
}

// TestLinkCostFormula verifies the Phase B cost: lossEff composes ping loss
// and egress drop; sojourn adds to SRTT; confidence divides.
func TestLinkCostFormula(t *testing.T) {
	// Base: srtt=100, no loss, full confidence → 100
	if c := linkCost(100, 0, 0, 0, 1.0); c != 100 {
		t.Fatalf("linkCost base = %v, want 100", c)
	}
	// 50% ping loss → denom = 0.5 → 200
	if c := linkCost(100, 0.5, 0, 0, 1.0); c != 200 {
		t.Fatalf("linkCost 50%% loss = %v, want 200", c)
	}
	// Loss so high the denom would underflow → clamps to 0.01 → 10000
	if c := linkCost(100, 0.999, 0, 0, 1.0); c != 10000 {
		t.Fatalf("linkCost 99.9%% loss = %v, want 10000", c)
	}
	// 10% ping loss + 10% drop → lossEff=0.19, denom=0.81
	if c := linkCost(100, 0.1, 0.1, 0, 1.0); math.Abs(c-100/0.81) > 1e-9 {
		t.Fatalf("linkCost composed loss = %v, want %v", c, 100/0.81)
	}
	// Sojourn adds to numerator
	if c := linkCost(100, 0, 0, 20, 1.0); c != 120 {
		t.Fatalf("linkCost with sojourn = %v, want 120", c)
	}
	// Confidence 0.1 → 10×
	if c := linkCost(100, 0, 0, 0, 0.1); c != 1000 {
		t.Fatalf("linkCost conf=0.1 = %v, want 1000", c)
	}
	// Confidence <=0 clamps to 0.01, never div-by-zero
	if c := linkCost(100, 0, 0, 0, 0); c != 10000 {
		t.Fatalf("linkCost conf=0 = %v, want 10000", c)
	}
}

// TestApplyHysteresisBoundary verifies A4: a new nextHop is adopted only
// when it beats the incumbent's CURRENT cost by ≥15%.
func TestApplyHysteresisBoundary(t *testing.T) {
	m := &MeshManager{nodeID: "self"}

	// Incumbent path self→B→T with per-link cost 25 each.
	old := &routeTable{bestPaths: map[string]dijkstraResult{
		"T": {NextHop: "B", Cost: 50, Path: []string{"B", "T"}},
	}}
	m.routeTable.Store(old)

	graph := &TopologyGraph{
		Nodes: map[string]bool{"self": true, "A": true, "B": true, "T": true},
		Edges: map[string]map[string][]*LinkQualityInfo{
			"self": {"A": {{Cost: 10}}, "B": {{Cost: 25}}},
			"A":    {"T": {{Cost: 25}}},
			"B":    {"T": {{Cost: 25}}},
		},
	}
	incumbent := 25 + hopPenaltyMs + 25 + hopPenaltyMs // 60

	cases := []struct {
		name      string
		newCost   float64
		wantHop   string // nextHop in result
	}{
		{"below hysteresis (better by >15%)", incumbent * 0.80, "A"},
		{"exactly at 15% improvement", incumbent * 0.85, "B"}, // not strictly better → keep
		{"just above threshold", incumbent * 0.851, "B"},
		{"just below threshold", incumbent * 0.849, "A"},
		{"worse path", incumbent * 1.2, "B"},
	}
	for _, tc := range cases {
		newPaths := map[string]dijkstraResult{
			"T": {NextHop: "A", Cost: tc.newCost, Path: []string{"A", "T"}},
		}
		got := m.applyHysteresis(graph, newPaths)["T"]
		if got.NextHop != tc.wantHop {
			t.Fatalf("%s: nextHop = %s, want %s (incumbent=%.1f new=%.1f)",
				tc.name, got.NextHop, tc.wantHop, incumbent, tc.newCost)
		}
	}
}

// TestApplyHysteresisDeadIncumbent verifies that a new path is adopted when
// the incumbent's links no longer exist in the graph.
func TestApplyHysteresisDeadIncumbent(t *testing.T) {
	m := &MeshManager{nodeID: "self"}
	m.routeTable.Store(&routeTable{bestPaths: map[string]dijkstraResult{
		"T": {NextHop: "B", Cost: 50, Path: []string{"B", "T"}},
	}})
	graph := &TopologyGraph{
		Nodes: map[string]bool{"self": true, "A": true, "T": true},
		Edges: map[string]map[string][]*LinkQualityInfo{
			"self": {"A": {{Cost: 10}}},
			"A":    {"T": {{Cost: 25}}},
		},
	}
	newPaths := map[string]dijkstraResult{
		"T": {NextHop: "A", Cost: 40, Path: []string{"A", "T"}},
	}
	if got := m.applyHysteresis(graph, newPaths)["T"]; got.NextHop != "A" {
		t.Fatalf("dead incumbent: nextHop = %s, want A", got.NextHop)
	}
}
