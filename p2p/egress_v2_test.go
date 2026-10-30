package p2p

import (
	"testing"
	"time"
)

// TestPingTrackerLatePongOverturn verifies Phase C correction: a pong
// arriving after the 5s timeout verdict but within the 30s grace overturns
// its loss sample, resets consecutiveLost, and records the full RTT.
func TestPingTrackerLatePongOverturn(t *testing.T) {
	tr := newPingTracker()

	// Ping seq=100 sent "6s ago" → recordPing ages it out as a loss.
	// (Use a high seq to avoid colliding with nextSeq=1.)
	tr.mu.Lock()
	tr.pendingPings[100] = time.Now().Add(-6 * time.Second)
	tr.mu.Unlock()

	seq := tr.recordPing()
	if seq != 1 {
		// nextSeq started at 1; the aged entry consumed seq 1 implicitly? No —
		// recordPing only ages, it does not consume the seq for aged pings.
		t.Fatalf("seq = %d, want 1", seq)
	}
	if tr.getLossRate() != 1.0 {
		t.Fatalf("lossRate after timeout = %v, want 1.0", tr.getLossRate())
	}
	if tr.consecutiveLostCount() != 1 {
		t.Fatalf("consecutiveLost = %d, want 1", tr.consecutiveLostCount())
	}

	// Late pong arrives (within grace): sample overturned.
	tr.recordPong(100, 0)
	if tr.getLossRate() != 0.0 {
		t.Fatalf("lossRate after late pong = %v, want 0.0", tr.getLossRate())
	}
	if tr.consecutiveLostCount() != 0 {
		t.Fatalf("consecutiveLost after late pong = %d, want 0", tr.consecutiveLostCount())
	}
	if _, _, _, _, sampleCount := tr.getStats(); sampleCount != 1 {
		t.Fatalf("sampleCount = %d, want 1 (full RTT recorded)", sampleCount)
	}
}

// TestPingTrackerGraceExpiry verifies a pong arriving after the 30s grace
// cannot overturn the verdict.
func TestPingTrackerGraceExpiry(t *testing.T) {
	tr := newPingTracker()

	tr.mu.Lock()
	tr.pendingPings[100] = time.Now().Add(-31 * time.Second)
	tr.mu.Unlock()

	tr.recordPing() // ages seq 100 into grace, then purges it (past grace)
	if tr.getLossRate() != 1.0 {
		t.Fatalf("lossRate = %v, want 1.0", tr.getLossRate())
	}
	tr.recordPong(100, 0) // too late: grace entry already purged
	if tr.getLossRate() != 1.0 {
		t.Fatalf("lossRate after expired pong = %v, want unchanged 1.0", tr.getLossRate())
	}
	if _, _, _, _, sampleCount := tr.getStats(); sampleCount != 0 {
		t.Fatalf("sampleCount = %d, want 0 (no RTT for expired pong)", sampleCount)
	}
}

// TestPingTrackerUnknownPong verifies an unknown/duplicate seq is ignored.
func TestPingTrackerUnknownPong(t *testing.T) {
	tr := newPingTracker()
	tr.recordPong(999, 0)
	if _, _, _, _, sampleCount := tr.getStats(); sampleCount != 0 {
		t.Fatalf("sampleCount = %d, want 0", sampleCount)
	}
}

// TestPingTrackerNormalPong verifies the in-flight fast path is unchanged.
func TestPingTrackerNormalPong(t *testing.T) {
	tr := newPingTracker()
	tr.recordPing()
	tr.recordPong(1, 0)
	if tr.getLossRate() != 0.0 {
		t.Fatalf("lossRate = %v, want 0", tr.getLossRate())
	}
	if tr.consecutiveLostCount() != 0 {
		t.Fatalf("consecutiveLost = %d, want 0", tr.consecutiveLostCount())
	}
}

// TestPingTrackerCancelPing verifies a rolled-back (enqueue-rejected) ping
// never enters the window.
func TestPingTrackerCancelPing(t *testing.T) {
	tr := newPingTracker()
	seq := tr.recordPing()
	tr.cancelPing(seq)
	if len(tr.samples) != 0 {
		t.Fatalf("samples = %d, want 0 after cancel", len(tr.samples))
	}
	tr.recordPong(seq, 0) // cancelled: neither pending nor grace → ignored
	if len(tr.samples) != 0 {
		t.Fatalf("samples = %d, want 0 after pong for cancelled ping", len(tr.samples))
	}
}

// TestSampleEgressWindow verifies Phase B drop-rate math: window rate =
// drops / (drops + sent), EMA-smoothed with alpha 0.5; an idle window
// decays both metrics toward zero.
func TestSampleEgressWindow(t *testing.T) {
	p := &Peer{}

	// Window 1: 10 sent, 0 drops → rate 0
	p.egressMu.Lock()
	p.framesSent = 10
	p.egressMu.Unlock()
	dropRate, _ := p.sampleEgressWindow()
	if dropRate != 0 {
		t.Fatalf("dropRate = %v, want 0", dropRate)
	}

	// Window 2: 5 sent, 5 drops → window rate 0.5, EMA = 0.5*0 + 0.5*0.5 = 0.25
	p.egressMu.Lock()
	p.framesSent = 5
	p.egressMu.Unlock()
	p.dnsQueueDrops = 5
	dropRate, _ = p.sampleEgressWindow()
	if dropRate != 0.25 {
		t.Fatalf("dropRate = %v, want 0.25", dropRate)
	}

	// Window 3: 5 sent, 5 drops again → EMA = 0.5*0.25 + 0.5*0.5 = 0.375
	p.egressMu.Lock()
	p.framesSent = 5
	p.egressMu.Unlock()
	p.dnsQueueDrops = 10
	dropRate, _ = p.sampleEgressWindow()
	if dropRate != 0.375 {
		t.Fatalf("dropRate = %v, want 0.375", dropRate)
	}

	// Window 4: idle → rate 0, EMA = 0.5*0.375 + 0.5*0 = 0.1875
	dropRate, sojourn := p.sampleEgressWindow()
	if dropRate != 0.1875 {
		t.Fatalf("idle dropRate = %v, want 0.1875", dropRate)
	}
	if sojourn != 0 {
		t.Fatalf("idle sojourn = %v, want 0", sojourn)
	}
}

// TestRecordSojournEMA verifies the sojourn EMA: first sample initializes,
// subsequent samples smooth with alpha 0.2; control frames are skipped.
func TestRecordSojournEMA(t *testing.T) {
	p := &Peer{}

	// Control frame: skipped.
	p.recordSojourn(writeReq{isControl: true, enqueueTime: time.Now().Add(-time.Second)})
	p.egressMu.Lock()
	if p.sojournSamples != 0 {
		t.Fatal("control frame counted in sojourn")
	}
	p.egressMu.Unlock()

	// First data sample initializes EMA.
	p.recordSojourn(writeReq{enqueueTime: time.Now().Add(-100 * time.Millisecond)})
	p.egressMu.Lock()
	if p.sojournEMA < 99 || p.sojournEMA > 101 {
		t.Fatalf("sojournEMA = %v, want ~100", p.sojournEMA)
	}
	p.egressMu.Unlock()

	// Second sample at 50ms → EMA = 100 + 0.2*(50-100) = 90.
	p.recordSojourn(writeReq{enqueueTime: time.Now().Add(-50 * time.Millisecond)})
	p.egressMu.Lock()
	if p.sojournEMA < 89 || p.sojournEMA > 91 {
		t.Fatalf("sojournEMA = %v, want ~90", p.sojournEMA)
	}
	p.egressMu.Unlock()

	// Zero enqueueTime (pre-Phase-B caller): skipped.
	p.recordSojourn(writeReq{})
	p.egressMu.Lock()
	if p.sojournSamples != 2 {
		t.Fatalf("sojournSamples = %d, want 2", p.sojournSamples)
	}
	p.egressMu.Unlock()
}
