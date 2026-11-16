package p2p

import (
	"testing"
	"time"
)

// TestPingTrackerPendingStats pins the signal used by the early carrier-stall
// reset: the oldest pending ping's age plus the number of pings still in
// flight. The caller requires >1 pending ping so a single isolated loss does
// not tear down the carrier.
func TestPingTrackerPendingStats(t *testing.T) {
	tr := newPingTracker()
	if age, n := tr.pendingStats(); n != 0 || age != 0 {
		t.Fatalf("empty tracker: age=%v n=%d, want 0/0", age, n)
	}

	tr.pendingPings[1] = time.Now().Add(-4 * time.Second)
	tr.pendingPings[2] = time.Now()

	age, n := tr.pendingStats()
	if n != 2 {
		t.Fatalf("pending count = %d, want 2", n)
	}
	if age < 3800*time.Millisecond || age > 6*time.Second {
		t.Fatalf("oldest age = %v, want about 4s", age)
	}
}
