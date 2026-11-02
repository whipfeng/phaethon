package mesh

import (
	"net"
	"testing"
	"time"
)

// feed mimics the monitor loop: record the sample, then evaluate.
func feed(sm *StallMonitor, base time.Time, sec int, a, b, d uint64) []int {
	s := sample{t: base.Add(time.Duration(sec) * time.Second), a: a, b: b, dVIP: d}
	sm.recordSample(s)
	return sm.evaluate(s, s.t)
}

func TestStallMonitorRule0_OutboundBlackhole(t *testing.T) {
	sm := NewStallMonitor(&MeshManager{}, nil, nil, "test", t.TempDir())
	base := time.Now()

	if f := feed(sm, base, 0, 0, 0, 0); len(f) != 0 {
		t.Fatalf("t=0 unexpected fire: %v", f)
	}
	// D flows at 10/s alongside A, then freezes at t=50.
	for sec := 10; sec <= 50; sec += 10 {
		if f := feed(sm, base, sec, uint64(sec), 0, uint64(sec*10)); len(f) != 0 {
			t.Fatalf("t=%d fired too early: %v", sec, f)
		}
	}
	// t=60: D frozen only 10s → no fire yet.
	if f := feed(sm, base, 60, 60, 0, 500); len(f) != 0 {
		t.Fatalf("t=60 fired before freezeWindow: %v", f)
	}
	// t=110: D frozen 60s, was flowing at 10/s before freezing, A moving → fire once.
	f := feed(sm, base, 110, 110, 0, 500)
	if len(f) != 1 || f[0] != 0 {
		t.Fatalf("t=110 expected [0], got %v", f)
	}
	// armed=false: suppressed while D stays frozen.
	for sec := 120; sec <= 170; sec += 10 {
		if f := feed(sm, base, sec, uint64(sec), 0, 500); len(f) != 0 {
			t.Fatalf("t=%d fired while disarmed: %v", sec, f)
		}
	}
	// D recovers at t=180 → re-arm; freezes again → fires again.
	feed(sm, base, 180, 180, 0, 600)
	if f := feed(sm, base, 240, 240, 0, 600); len(f) != 1 || f[0] != 0 {
		t.Fatalf("t=240 expected re-fire [0], got %v", f)
	}
}

// TestStallMonitorRule0_ClientRoleSparseD is the 2026-11-02 false-positive
// regression: on a client-role node D (peer-initiated frames to local VIP) is
// naturally sparse. A rising with D flat must NOT fire.
func TestStallMonitorRule0_ClientRoleSparseD(t *testing.T) {
	sm := NewStallMonitor(&MeshManager{}, nil, nil, "test", t.TempDir())
	base := time.Now()

	feed(sm, base, 0, 0, 0, 0)
	for sec := 10; sec <= 300; sec += 10 {
		// A active, D sparse (5 frames total, never reaching minTargetActivity).
		if f := feed(sm, base, sec, uint64(sec*100), 0, 5); len(f) != 0 {
			t.Fatalf("t=%d false positive on sparse-D client role: %v", sec, f)
		}
	}
}

func TestStallMonitorRule1_DeliveryStall(t *testing.T) {
	sm := NewStallMonitor(&MeshManager{}, nil, nil, "test", t.TempDir())
	base := time.Now()

	feed(sm, base, 0, 0, 0, 0)
	// B flows at 10/s, D rising; B freezes at t=50.
	for sec := 10; sec <= 50; sec += 10 {
		if f := feed(sm, base, sec, 0, uint64(sec*10), uint64(sec)); len(f) != 0 {
			t.Fatalf("t=%d fired too early: %v", sec, f)
		}
	}
	if f := feed(sm, base, 60, 0, 500, 60); len(f) != 0 {
		t.Fatalf("t=60 fired before freezeWindow: %v", f)
	}
	f := feed(sm, base, 110, 0, 500, 110)
	if len(f) != 1 || f[0] != 1 {
		t.Fatalf("t=110 expected [1], got %v", f)
	}
}

func TestStallMonitorRule2_ReadLoopOrHostSilent(t *testing.T) {
	sm := NewStallMonitor(&MeshManager{}, nil, nil, "test", t.TempDir())
	base := time.Now()

	feed(sm, base, 0, 0, 0, 0)
	// A flows at 10/s (with D moving once, as real host-facing returns
	// would), B rising; A freezes at t=50.
	for sec := 10; sec <= 50; sec += 10 {
		if f := feed(sm, base, sec, uint64(sec*10), uint64(sec), 3); len(f) != 0 {
			t.Fatalf("t=%d fired too early: %v", sec, f)
		}
	}
	if f := feed(sm, base, 60, 500, 60, 3); len(f) != 0 {
		t.Fatalf("t=60 fired before freezeWindow: %v", f)
	}
	f := feed(sm, base, 110, 500, 110, 3)
	if len(f) != 1 || f[0] != 2 {
		t.Fatalf("t=110 expected [2], got %v", f)
	}
}

func TestStallMonitorIdleNeverFires(t *testing.T) {
	sm := NewStallMonitor(&MeshManager{}, nil, nil, "test", t.TempDir())
	base := time.Now()

	feed(sm, base, 0, 0, 0, 0)
	for sec := 10; sec <= 300; sec += 10 {
		if f := feed(sm, base, sec, 0, 0, 0); len(f) != 0 {
			t.Fatalf("idle fired at t=%d: %v", sec, f)
		}
	}
}

func TestBuildSYNChecksums(t *testing.T) {
	src := net.ParseIP("100.64.1.1").To4()
	dst := net.ParseIP("100.64.1.2").To4()
	b := buildSYN(src, dst, 40555, 9)
	if got := finalizeSum(addBytes(0, b[0:20])); got != 0 {
		t.Fatalf("ipv4 checksum not zero-fold (invalid): 0x%x", got)
	}
	sum := pseudoHeaderSum(src, dst, 6, 20)
	sum = addBytes(sum, b[20:40])
	if got := finalizeSum(sum); got != 0 {
		t.Fatalf("tcp checksum not zero-fold (invalid): 0x%x", got)
	}
	if b[9] != 6 || b[33] != 2 {
		t.Fatalf("not a TCP SYN: proto=%d flags=%02x", b[9], b[33])
	}
}
