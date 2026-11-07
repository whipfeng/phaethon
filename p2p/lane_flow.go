package p2p

import (
	"encoding/binary"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"phaethon/frame"
	"phaethon/mesh"
	"phaethon/util"
)

// Lane flow accounting (docs/plans/mesh-control-isolation-inflight-cap.md §3):
// the sender counts payload bytes written per lane, the receiver counts
// payload bytes read per lane and reports the cumulative count back with
// FrameLaneAck. tx − ackedRx is the backlog sitting outside this process
// (carrier buffers, proxies, network).

const (
	laneAckInterval    = 50 * time.Millisecond
	laneRTTSampleGap   = 10 * time.Millisecond
	laneRTTRingCap     = 256
	laneRateWindow     = 200 * time.Millisecond
	laneMinRTTBucket   = time.Minute
	laneMinRTTBuckets  = 10
	laneDiagInterval   = 5 * time.Second
	laneDiagInflight   = 64 << 10
	laneDiagRTT        = 500 * time.Millisecond
	laneAckEntrySize   = 12
	laneAckMaxEntries  = 64
	laneIDPrimary      = uint32(0)
	laneAckFrameType   = frame.FrameLaneAck
	laneGaugeFrameType = frame.FrameLaneGauge
)

// In-flight cap tunables (§3.3 LEDBAT-like, delay-driven).
const (
	laneCapInitial        = 256 << 10  // 256KB starting cap
	laneCapMin            = 64 << 10   // never throttle below this
	laneCapMax            = 8 << 20    // never exceed this
	laneQDTarget          = 200 * time.Millisecond
	laneCapAdjustInterval = 200 * time.Millisecond
	laneNegQDResetRounds  = 25 // 25 × 200ms = 5s of persistent negative qd → rebuild minRTT baseline
	laneStallTimeout      = 10 * time.Second
)

type laneTxSample struct {
	at time.Time
	tx uint64
}

type laneFlow struct {
	tx atomic.Uint64 // payload bytes written on this lane
	rx atomic.Uint64 // payload bytes read on this lane

	mu         sync.Mutex
	acked      uint64 // peer-reported rx
	gated      bool   // at least one ack seen: the peer supports LaneAck
	samples    [laneRTTRingCap]laneTxSample
	head, n    int
	lastSample time.Time
	srtt       time.Duration
	lastRTT    time.Duration
	minBuckets [laneMinRTTBuckets]time.Duration
	minBucketAt time.Time
	rate       float64 // bytes/s delivered (max-biased EMA)
	rateAcked  uint64
	rateAt     time.Time
	lastAckAt  time.Time
	rxReported uint64 // receiver side: last rx sent in an ack

	// In-flight cap (§3 B2). capVal is mu-guarded: adjustCap writes it from
	// flowLoop, gateState reads it from peerWriteLoop.
	capVal          int64
	lastAdjustAt    time.Time
	negQDRounds     int
	ackedAdvancedAt time.Time // last time ackedRx actually advanced
	stallWarnedAt   time.Time
}

func (f *laneFlow) onSent(n int, now time.Time) {
	tx := f.tx.Add(uint64(n))
	f.mu.Lock()
	if now.Sub(f.lastSample) >= laneRTTSampleGap && f.n < laneRTTRingCap {
		f.samples[(f.head+f.n)%laneRTTRingCap] = laneTxSample{at: now, tx: tx}
		f.n++
		f.lastSample = now
	}
	f.mu.Unlock()
}

func (f *laneFlow) onRecv(n int) { f.rx.Add(uint64(n)) }

// onAck folds one peer-reported cumulative rx count into the flow.
func (f *laneFlow) onAck(rx uint64, now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if tx := f.tx.Load(); rx > tx {
		rx = tx
	}
	if rx < f.acked {
		return
	}
	if rx > f.acked {
		f.ackedAdvancedAt = now
	}
	f.acked = rx
	f.gated = true
	f.lastAckAt = now
	var rtt time.Duration
	for f.n > 0 && f.samples[f.head].tx <= rx {
		rtt = now.Sub(f.samples[f.head].at)
		f.head = (f.head + 1) % laneRTTRingCap
		f.n--
	}
	if rtt > 0 {
		f.lastRTT = rtt
		if f.srtt == 0 {
			f.srtt = rtt
		} else {
			f.srtt += (rtt - f.srtt) / 8
		}
		f.foldMinRTTLocked(rtt, now)
	}
	if f.rateAt.IsZero() {
		f.rateAt, f.rateAcked = now, rx
	} else if dt := now.Sub(f.rateAt); dt >= laneRateWindow {
		s := float64(rx-f.rateAcked) / dt.Seconds()
		if s >= f.rate {
			f.rate = s
		} else {
			f.rate += 0.25 * (s - f.rate)
		}
		f.rateAt, f.rateAcked = now, rx
	}
}

// foldMinRTTLocked keeps per-minute RTT minima over the last 10 minutes
// (LEDBAT base-delay history).
func (f *laneFlow) foldMinRTTLocked(rtt time.Duration, now time.Time) {
	if f.minBucketAt.IsZero() || now.Sub(f.minBucketAt) >= laneMinRTTBucket {
		copy(f.minBuckets[1:], f.minBuckets[:laneMinRTTBuckets-1])
		f.minBuckets[0] = rtt
		f.minBucketAt = now
		return
	}
	if rtt < f.minBuckets[0] {
		f.minBuckets[0] = rtt
	}
}

func (f *laneFlow) minRTTLocked() time.Duration {
	var m time.Duration
	for _, b := range f.minBuckets {
		if b > 0 && (m == 0 || b < m) {
			m = b
		}
	}
	return m
}

// inflight returns tx − ackedRx and whether the peer acks this lane.
func (f *laneFlow) inflight() (int64, bool) {
	tx := f.tx.Load()
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.gated {
		return 0, false
	}
	return int64(tx - f.acked), true
}

// --- in-flight cap (§3.3 LEDBAT-like, delay-driven) ---

// adjustCap recomputes the in-flight cap from the queueing delay:
//
//	qd = srtt − minRTT(10min)          // clamped ≥ 0
//	qd > target       → cap ×= max(0.5, target/qd)   // congested: shrink
//	qd < target/2 and inflight ≥ cap/2 → cap ×= 1.25 // idle but busy: grow
//
// qd < 0 (srtt below the polluted baseline after the gate takes effect) is
// clamped to 0 and never accelerates; persistent negative qd rebuilds the
// minRTT baseline from current samples (§3.3 负 qd 防护).
func (f *laneFlow) adjustCap(now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.gated {
		return
	}
	if !f.lastAdjustAt.IsZero() && now.Sub(f.lastAdjustAt) < laneCapAdjustInterval {
		return
	}
	minRTT := f.minRTTLocked()
	if minRTT <= 0 || f.srtt <= 0 {
		return
	}
	neg := f.srtt < minRTT
	qd := f.srtt - minRTT
	if qd < 0 {
		qd = 0
	}
	cap := f.capVal
	if cap == 0 {
		cap = laneCapInitial
	}
	switch {
	case qd > laneQDTarget:
		scale := float64(laneQDTarget) / float64(qd)
		if scale < 0.5 {
			scale = 0.5
		}
		cap = int64(float64(cap) * scale)
	case !neg && qd < laneQDTarget/2 && int64(f.tx.Load()-f.acked) >= cap/2:
		cap += cap / 4
	}
	if cap < laneCapMin {
		cap = laneCapMin
	}
	if cap > laneCapMax {
		cap = laneCapMax
	}
	f.capVal = cap
	f.lastAdjustAt = now

	if neg {
		if f.negQDRounds++; f.negQDRounds >= laneNegQDResetRounds {
			for i := range f.minBuckets {
				f.minBuckets[i] = 0
			}
			f.minBuckets[0] = f.srtt
			f.minBucketAt = now
			f.negQDRounds = 0
		}
	} else {
		f.negQDRounds = 0
	}
}

// gateState returns (cap, inflight, gated): the write gate is active only
// once the peer has acked at least once (gated).
func (f *laneFlow) gateState() (int64, int64, bool) {
	tx := f.tx.Load()
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.gated {
		return 0, 0, false
	}
	cap := f.capVal
	if cap == 0 {
		cap = laneCapInitial
	}
	return cap, int64(tx - f.acked), true
}

func (f *laneFlow) isGated() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gated
}

// stallCheck is the §3.3 stall guard: acks keep arriving but ackedRx no
// longer advances while data is still in flight — the gated bytes are not
// reaching the peer, so holding the gate would wedge the session. Disengage
// gating for this flow (a later ack re-engages it) and warn once.
// A slow-but-healthy link keeps ackedRx advancing (rx is re-reported on
// every change), so it never trips here.
func (f *laneFlow) stallCheck(now time.Time, laneID uint32, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.gated || f.ackedAdvancedAt.IsZero() || !f.stallWarnedAt.IsZero() {
		return
	}
	if int64(f.tx.Load()-f.acked) <= 0 {
		return
	}
	if now.Sub(f.ackedAdvancedAt) < laneStallTimeout {
		return
	}
	f.stallWarnedAt = now
	f.gated = false
	util.LogModuleWarn("p2p", "[LANE-STALLED] %s lane=%d inflight=%dKB ackedRx frozen %v, gate disengaged",
		name, laneID, int64(f.tx.Load()-f.acked)>>10, now.Sub(f.ackedAdvancedAt).Round(time.Second))
}

type laneFlowSnapshot struct {
	tx, inflight         int64
	cap                  int64
	gated                bool
	srtt, lastRTT, minRTT time.Duration
	rate                 float64
}

func (f *laneFlow) snapshot() laneFlowSnapshot {
	tx := f.tx.Load()
	f.mu.Lock()
	defer f.mu.Unlock()
	s := laneFlowSnapshot{
		tx: int64(tx), gated: f.gated, srtt: f.srtt, lastRTT: f.lastRTT,
		minRTT: f.minRTTLocked(), rate: f.rate,
	}
	if f.gated {
		s.inflight = int64(tx - f.acked)
		s.cap = f.capVal
		if s.cap == 0 {
			s.cap = laneCapInitial
		}
	}
	return s
}

// --- wire format ---

type laneAckEntry struct {
	laneID uint32
	rx     uint64
}

func encodeLaneAck(entries []laneAckEntry) []byte {
	b := make([]byte, 0, len(entries)*laneAckEntrySize)
	for _, e := range entries {
		b = binary.BigEndian.AppendUint32(b, e.laneID)
		b = binary.BigEndian.AppendUint64(b, e.rx)
	}
	return b
}

func decodeLaneAck(p []byte) []laneAckEntry {
	n := len(p) / laneAckEntrySize
	out := make([]laneAckEntry, 0, n)
	for i := 0; i < n; i++ {
		o := i * laneAckEntrySize
		out = append(out, laneAckEntry{
			laneID: binary.BigEndian.Uint32(p[o:]),
			rx:     binary.BigEndian.Uint64(p[o+4:]),
		})
	}
	return out
}

// --- stripe integration ---

// handleLaneAck processes a FrameLaneAck read on the primary lane: a
// non-empty payload carries our lanes' cumulative rx (§3.2 — the B1 empty
// payload request semantics is abolished; wantTx is declared via gossip).
func (t *stripeTransport) handleLaneAck(payload []byte) {
	if len(payload) == 0 {
		return
	}
	now := time.Now()
	for _, e := range decodeLaneAck(payload) {
		if f := t.flowByID(e.laneID); f != nil {
			f.onAck(e.rx, now)
		}
	}
	t.notifyCredit()
}

// handleLaneGauge processes a FrameLaneGauge urgent announcement: one byte,
// our congestion gauge scaled to 0-255 (§3.2). The periodic fallback rides
// gossip (WantGauge declaration); this frame is the threshold-crossing alert.
func (t *stripeTransport) handleLaneGauge(payload []byte) {
	if len(payload) != 1 {
		return
	}
	t.peerGauge.Store(int32(payload[0]))
}

func (t *stripeTransport) flowByID(id uint32) *laneFlow {
	if id == laneIDPrimary {
		return &t.primaryFlow
	}
	for _, l := range t.snapshotLanes() {
		if l.id == id {
			return &l.flow
		}
	}
	return nil
}

// requestLaneAcks is gone (§3.2 终审): the empty-payload trigger was a
// handshake; capability/want declarations now ride gossip (LaneAck/WantTx),
// and ackRequested is set from the peer's gossip WantTx declaration via
// onPeerLaneGossip.

// flowLoop sends lane acks while the peer asked for them, adjusts each
// lane's in-flight cap, checks the stall guard, and logs backlog diagnostics.
func (t *stripeTransport) flowLoop() {
	tick := time.NewTicker(laneAckInterval)
	defer tick.Stop()
	lastDiag := time.Now()
	for {
		select {
		case <-t.closed:
			return
		case now := <-tick.C:
			if t.ackRequested.Load() && t.cfg.sendControl != nil {
				t.sendLaneAcks()
			}
			t.primaryFlow.adjustCap(now)
			t.primaryFlow.stallCheck(now, laneIDPrimary, t.cfg.name)
			lanes := t.snapshotLanes()
			for _, l := range lanes {
				l.flow.adjustCap(now)
				l.flow.stallCheck(now, l.id, t.cfg.name)
			}
			t.notifyCredit()
			if now.Sub(lastDiag) >= laneDiagInterval {
				lastDiag = now
				t.logLaneDiag()
			}
		}
	}
}

func (t *stripeTransport) sendLaneAcks() {
	var entries []laneAckEntry
	add := func(id uint32, f *laneFlow) {
		rx := f.rx.Load()
		f.mu.Lock()
		changed := rx != f.rxReported
		f.rxReported = rx
		f.mu.Unlock()
		if changed && len(entries) < laneAckMaxEntries {
			entries = append(entries, laneAckEntry{laneID: id, rx: rx})
		}
	}
	add(laneIDPrimary, &t.primaryFlow)
	for _, l := range t.snapshotLanes() {
		add(l.id, &l.flow)
	}
	if len(entries) == 0 {
		return
	}
	if err := t.cfg.sendControl(laneAckFrameType, encodeLaneAck(entries)); err != nil {
		util.LogModuleDebug("p2p", "[LANE-ACK] ack send failed: %v", err)
	}
}

func (t *stripeTransport) logLaneDiag() {
	log := func(id uint32, f *laneFlow, prevTx *int64) {
		s := f.snapshot()
		moved := s.tx != *prevTx
		*prevTx = s.tx
		if !s.gated || !moved || (s.inflight < laneDiagInflight && s.lastRTT < laneDiagRTT) {
			return
		}
		util.LogModuleInfo("p2p", "[LANE-INFLIGHT] %s lane=%d inflight=%dKB cap=%dKB rtt=%v srtt=%v minRTT=%v rate=%.1fKB/s",
			t.cfg.name, id, s.inflight>>10, s.cap>>10, s.lastRTT.Round(time.Millisecond), s.srtt.Round(time.Millisecond),
			s.minRTT.Round(time.Millisecond), s.rate/1024)
	}
	log(laneIDPrimary, &t.primaryFlow, &t.diagPrimaryTx)
	for _, l := range t.snapshotLanes() {
		log(l.id, &l.flow, &l.diagTx)
	}
}

// --- write gate (§3.4) ---

// writeCredit is the session-level data credit peerWriteLoop gates on:
// Σ lane caps − Σ lane inflights over gated lanes. math.MaxInt64 when no
// lane is gated yet (peer never acked / doesn't support LaneAck) — the gate
// stays open and behavior matches the pre-B2 era.
func (t *stripeTransport) writeCredit() int64 {
	var credit int64
	any := false
	consider := func(f *laneFlow) {
		cap, inflight, gated := f.gateState()
		if !gated {
			return
		}
		if !any {
			any = true
			credit = 0
		}
		credit += cap - inflight
	}
	consider(&t.primaryFlow)
	for _, l := range t.snapshotLanes() {
		consider(&l.flow)
	}
	if !any {
		return math.MaxInt64
	}
	return credit
}

// notifyCredit wakes the peerWriteLoop to re-check its credit after an ack
// or a cap adjustment.
func (t *stripeTransport) notifyCredit() {
	if t.cfg.onCredit != nil {
		t.cfg.onCredit()
	}
}

// --- peer telemetry via gossip (§3.2) ---

// onPeerLaneGossip folds the peer's LaneAck gossip fields into this
// transport: the WantTx declaration arms our rx reporting (no negotiation
// handshake — the declaration IS the negotiation), LaneRx entries fold into
// the per-lane flows through the same onAck path as FrameLaneAck, Gauge is
// the peer's congestion telemetry (§2.6, consumed by the A-phase lane
// controller).
func (t *stripeTransport) onPeerLaneGossip(wantTx, wantGauge bool, rx []laneAckEntry, gauge int) {
	if wantTx && !t.ackRequested.Swap(true) {
		util.LogModuleInfo("p2p", "[LANE-ACK] %s peer wants lane rx reports (gossip wantTx)", t.cfg.name)
	}
	t.peerWantsTx.Store(wantTx)
	t.peerWantsGauge.Store(wantGauge)
	if gauge > 0 {
		t.peerGauge.Store(int32(gauge))
	}
	now := time.Now()
	for _, e := range rx {
		if f := t.flowByID(e.laneID); f != nil {
			f.onAck(e.rx, now)
		}
	}
	t.notifyCredit()
}

// laneGossipFields builds this link's per-peer gossip injection (same
// contract as GossipFields): declarations always; rx reports only when the
// peer asked (WantTx), gauge only when it asked (WantGauge).
func (t *stripeTransport) laneGossipFields() (capable, wantTx, wantGauge bool, rx []mesh.GossipLaneRx, gauge int) {
	if t.cfg.sendControl == nil {
		return false, false, false, nil, 0
	}
	wantTx, wantGauge = true, true
	if t.peerWantsTx.Load() {
		add := func(id uint32, f *laneFlow) {
			if v := f.rx.Load(); v > 0 && len(rx) < laneAckMaxEntries {
				rx = append(rx, mesh.GossipLaneRx{ID: id, Rx: v})
			}
		}
		add(laneIDPrimary, &t.primaryFlow)
		for _, l := range t.snapshotLanes() {
			add(l.id, &l.flow)
		}
	}
	if t.peerWantsGauge.Load() {
		t.ctrlMu.Lock()
		gauge = int(t.gauge * 255)
		t.ctrlMu.Unlock()
		if gauge > 255 {
			gauge = 255
		}
	}
	return true, wantTx, wantGauge, rx, gauge
}

// announceGaugeLocked sends an urgent FrameLaneGauge when the gauge crosses
// the high watermark in either direction (§3.2). Must be called with ctrlMu
// held (gauge just recomputed).
func (t *stripeTransport) announceGaugeLocked() {
	if t.cfg.sendControl == nil {
		return
	}
	prev := t.gaugeSent
	cur := t.gauge
	if !((prev < stripeGaugeHighWatermark && cur >= stripeGaugeHighWatermark) ||
		(prev >= stripeGaugeHighWatermark && cur < stripeGaugeHighWatermark)) {
		return
	}
	t.gaugeSent = cur
	g := byte(cur * 255)
	if err := t.cfg.sendControl(laneGaugeFrameType, []byte{g}); err != nil {
		util.LogModuleDebug("p2p", "[LANE-GAUGE] %s announce failed: %v", t.cfg.name, err)
	}
}
