package p2p

import (
	"encoding/binary"
	"sync"
	"sync/atomic"
	"time"

	"phaethon/frame"
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

type laneFlowSnapshot struct {
	tx, inflight         int64
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

// handleLaneAck processes a FrameLaneAck read on the primary lane: an empty
// payload is the peer asking for acks, otherwise it carries our lanes' rx.
func (t *stripeTransport) handleLaneAck(payload []byte) {
	if len(payload) == 0 {
		if !t.ackRequested.Swap(true) {
			util.LogModuleInfo("p2p", "[LANE-ACK] peer requested lane acks")
		}
		return
	}
	now := time.Now()
	for _, e := range decodeLaneAck(payload) {
		if f := t.flowByID(e.laneID); f != nil {
			f.onAck(e.rx, now)
		}
	}
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

// requestLaneAcks asks the peer (once per transport) to report lane rx.
// Deferred to the first mesh data send so it never precedes the session
// handshake; an old peer logs the unknown frame once and ignores it.
func (t *stripeTransport) requestLaneAcks() {
	if t.cfg.sendControl == nil || t.ackRequestSent.Swap(true) {
		return
	}
	go func() {
		if err := t.cfg.sendControl(laneAckFrameType, nil); err != nil {
			util.LogModuleDebug("p2p", "[LANE-ACK] request send failed: %v", err)
			t.ackRequestSent.Store(false)
		}
	}()
}

// flowLoop sends lane acks while the peer asked for them and logs backlog
// diagnostics for our own lanes.
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
		util.LogModuleInfo("p2p", "[LANE-INFLIGHT] %s lane=%d inflight=%dKB rtt=%v srtt=%v minRTT=%v rate=%.1fKB/s",
			t.cfg.name, id, s.inflight>>10, s.lastRTT.Round(time.Millisecond), s.srtt.Round(time.Millisecond),
			s.minRTT.Round(time.Millisecond), s.rate/1024)
	}
	log(laneIDPrimary, &t.primaryFlow, &t.diagPrimaryTx)
	for _, l := range t.snapshotLanes() {
		log(l.id, &l.flow, &l.diagTx)
	}
}
