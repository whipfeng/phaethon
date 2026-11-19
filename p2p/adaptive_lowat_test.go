package p2p

import (
	"testing"
	"time"

	"phaethon/frame"
)

func TestComputeAdaptiveLowatFloor(t *testing.T) {
	now := time.Now()
	got, _ := computeAdaptiveLowat(16<<10, 0, time.Time{}, now)
	if got != lowatFloor {
		t.Fatalf("first cap = %d, want floor %d", got, lowatFloor)
	}
	// A low target must never shrink below the platform floor.
	got, _ = computeAdaptiveLowat(1<<10, 128<<10, now.Add(-10*time.Second), now)
	if got < lowatFloor {
		t.Fatalf("cap = %d, want >= floor %d", got, lowatFloor)
	}
}

func TestComputeAdaptiveLowatGrowFastShrinkSlow(t *testing.T) {
	now := time.Now()
	cur := 128 << 10

	// Growth is immediate.
	got, _ := computeAdaptiveLowat(200<<10, cur, time.Time{}, now)
	if got != 200<<10 {
		t.Fatalf("grow cap = %d, want %d", got, 200<<10)
	}

	// A one-tick low reading only arms the shrink timer.
	lowSince := time.Time{}
	got, lowSince = computeAdaptiveLowat(30<<10, cur, lowSince, now)
	if got != cur {
		t.Fatalf("premature shrink: cap = %d, want %d", got, cur)
	}
	if lowSince.IsZero() {
		t.Fatal("shrink timer not armed")
	}

	// After the sustain window, shrink by 25% (not straight to target).
	got, _ = computeAdaptiveLowat(30<<10, cur, lowSince, now.Add(lowatShrinkAfter+time.Second))
	want := cur - cur/4
	if got != want {
		t.Fatalf("slow shrink cap = %d, want %d", got, want)
	}
}

type fakeStreamTransport struct{}

func (fakeStreamTransport) Send(byte, []byte, bool) error { return nil }
func (fakeStreamTransport) SendBatch([]frame.Frame) error  { return nil }
func (fakeStreamTransport) Recv() (byte, []byte, error)    { return 0, nil, nil }
func (fakeStreamTransport) Close() error                   { return nil }
func (fakeStreamTransport) IsStreamCarrier() bool          { return true }

type fakePoolTransport struct{}

func (fakePoolTransport) Send(byte, []byte, bool) error { return nil }
func (fakePoolTransport) SendBatch([]frame.Frame) error  { return nil }
func (fakePoolTransport) Recv() (byte, []byte, error)    { return 0, nil, nil }
func (fakePoolTransport) Close() error                   { return nil }

func TestWarmLanesAutomaticForStreamCarrier(t *testing.T) {
	t.Setenv("PHAETHON_LAB", "")
	tr := &stripeTransport{
		base: fakeStreamTransport{},
		cfg:  stripeConfig{dialLane: func() (uint32, frame.FrameTransport, error) { return 1, nil, nil }},
	}
	if got := tr.warmLanes(); got != 2 {
		t.Fatalf("stream carrier warm lanes = %d, want 2", got)
	}

	// Passive side cannot dial lanes.
	tr.cfg.dialLane = nil
	if got := tr.warmLanes(); got != 0 {
		t.Fatalf("passive warm lanes = %d, want 0", got)
	}

	// Independent HTTP pools (long-poll) already multiplex; no warm lanes.
	tr.cfg.dialLane = func() (uint32, frame.FrameTransport, error) { return 1, nil, nil }
	tr.base = fakePoolTransport{}
	if got := tr.warmLanes(); got != 0 {
		t.Fatalf("pool carrier warm lanes = %d, want 0", got)
	}
}
