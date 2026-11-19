package p2p

import (
	"testing"
	"time"
)

func TestLanePacerZeroRateIsPassthrough(t *testing.T) {
	p := newLanePacer()
	start := time.Now()
	if !p.wait(1<<20, nil, nil) {
		t.Fatal("wait returned false")
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Fatalf("zero-rate wait blocked for %v", time.Since(start))
	}
}

func TestLanePacerClamps(t *testing.T) {
	p := newLanePacer()
	p.setRate(1)
	p.mu.Lock()
	got := p.rate
	p.mu.Unlock()
	if got != lanePaceFloor {
		t.Fatalf("floor rate = %v, want %v", got, lanePaceFloor)
	}

	p.setRate(1 << 40)
	p.mu.Lock()
	got = p.rate
	p.mu.Unlock()
	if got != lanePaceCeil {
		t.Fatalf("ceil rate = %v, want %v", got, lanePaceCeil)
	}

	p.setRate(0)
	p.mu.Lock()
	got = p.rate
	p.mu.Unlock()
	if got != 0 {
		t.Fatalf("zero rate = %v, want 0", got)
	}
}

func TestLanePacerStopsOnLaneClose(t *testing.T) {
	p := newLanePacer()
	p.setRate(lanePaceFloor) // 512KB/s: a 1MB wait would block ~2s
	done := make(chan struct{})
	close(done)
	start := time.Now()
	if p.wait(1<<20, done, nil) {
		t.Fatal("wait returned true for a closed lane")
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatalf("closed lane wait blocked for %v", time.Since(start))
	}
}

func TestLanePacerPacesAtRate(t *testing.T) {
	p := newLanePacer()
	p.setRate(8 << 20) // 8MB/s
	start := time.Now()
	if !p.wait(64<<10, nil, nil) {
		t.Fatal("wait returned false")
	}
	elapsed := time.Since(start)
	// 64KB at 8MB/s is ~8ms; allow a wide bound for CI scheduling.
	if elapsed < 2*time.Millisecond || elapsed > 200*time.Millisecond {
		t.Fatalf("paced wait took %v, want ~8ms", elapsed)
	}
}
