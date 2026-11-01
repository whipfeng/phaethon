package mesh

import (
	"net"
	"testing"
	"time"
)

// feed drives evaluate with a synthetic sample. d lands in dVIP; b and a are
// carried directly so the test needs no netstack/tun.
func feed(sm *StallMonitor, base time.Time, sec int, a, b, d uint64) []int {
	s := sample{t: base.Add(time.Duration(sec) * time.Second), a: a, b: b, dVIP: d}
	return sm.evaluate(s, s.t)
}

func TestStallMonitorRule0_OutboundBlackhole(t *testing.T) {
	sm := NewStallMonitor(&MeshManager{}, nil, nil, "test", t.TempDir())
	base := time.Now()

	if f := feed(sm, base, 0, 0, 0, 0); len(f) != 0 {
		t.Fatalf("t=0 unexpected fire: %v", f)
	}
	// A keeps rising, D never moves.
	for sec := 10; sec <= 50; sec += 10 {
		if f := feed(sm, base, sec, uint64(sec), 0, 0); len(f) != 0 {
			t.Fatalf("t=%d fired too early: %v", sec, f)
		}
	}
	// t=60: D frozen 60s, A moved recently → rule 0 fires exactly once.
	f := feed(sm, base, 60, 60, 0, 0)
	if len(f) != 1 || f[0] != 0 {
		t.Fatalf("t=60 expected [0], got %v", f)
	}
	// armed=false: keeps firing suppressed while D stays frozen.
	for sec := 80; sec <= 140; sec += 10 {
		if f := feed(sm, base, sec, uint64(sec), 0, 0); len(f) != 0 {
			t.Fatalf("t=%d fired while disarmed: %v", sec, f)
		}
	}
	// D recovers at t=150 → re-arm; freezes again → fires again.
	feed(sm, base, 150, 150, 0, 3)
	if f := feed(sm, base, 220, 220, 0, 3); len(f) != 1 || f[0] != 0 {
		t.Fatalf("t=220 expected re-fire [0], got %v", f)
	}
}

func TestStallMonitorRule1_DeliveryStall(t *testing.T) {
	sm := NewStallMonitor(&MeshManager{}, nil, nil, "test", t.TempDir())
	base := time.Now()

	feed(sm, base, 0, 0, 0, 0)
	// D rises, B frozen.
	for sec := 10; sec <= 50; sec += 10 {
		if f := feed(sm, base, sec, 0, 0, uint64(sec)); len(f) != 0 {
			t.Fatalf("t=%d fired too early: %v", sec, f)
		}
	}
	f := feed(sm, base, 60, 0, 0, 60)
	if len(f) != 1 || f[0] != 1 {
		t.Fatalf("t=60 expected [1], got %v", f)
	}
}

func TestStallMonitorRule2_ReadLoopOrHostSilent(t *testing.T) {
	sm := NewStallMonitor(&MeshManager{}, nil, nil, "test", t.TempDir())
	base := time.Now()

	feed(sm, base, 0, 0, 0, 0)
	// B rises (with D moving once, as real host-facing returns would), A frozen.
	for sec := 10; sec <= 50; sec += 10 {
		if f := feed(sm, base, sec, 0, uint64(sec), 3); len(f) != 0 {
			t.Fatalf("t=%d fired too early: %v", sec, f)
		}
	}
	f := feed(sm, base, 60, 0, 60, 3)
	if len(f) != 1 || f[0] != 2 {
		t.Fatalf("t=60 expected [2], got %v", f)
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
