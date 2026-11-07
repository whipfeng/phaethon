package p2p

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"phaethon/frame"
	"phaethon/util"
)

// stripeTransport overlays dynamic extra data lanes (stripes) on the primary
// proxy connection within a single P2P session
// (docs/plans/mesh-multilink-stripe-design.md §5). It presents one
// FrameTransport to the peer layer — lanes are invisible to routing, gossip
// and DRR; splitting happens inside SendBatch, fan-in inside Recv.
//
// Write path (§5.3): only FrameMeshPacket frames are split, by inner-5-tuple
// hash across {primary ∪ dynamic lanes}; control/DNS frames always ride the
// primary lane. Each lane owns an outbound queue drained by a dedicated
// writer goroutine, so one wedged lane cannot backpressure the dispatcher
// (and through it the single peerWriteLoop) while other lanes are idle. A
// full or dead lane falls back to the primary lane — never a drop on a TCP leg.
//
// Read path (§5.5): the primary and every lane pump into shared ctrl/data
// channels with control priority preserved (udp_leg_transport pattern). A
// control/gossip frame arriving on a lane is a protocol error: warn + drop.
// LinkPing/LinkPong on a lane are consumed by the lane's own probe machinery
// (§2.1): pings get an immediate pong on the same lane, pongs match
// outstanding probes and are never surfaced to the session.
//
// Dynamic controller (§2, tasks 2.x): the active side watches an EMA of the
// primary lane's send sojourn against its own sliding-min baseline — a
// dimensionless gauge in [0,1] (§5.4 self-normalized, BBR RTprop shape; no
// absolute ms threshold crosses environments). Sustained congestion
// (≥0.7 for 30s, ≥10s since the last add, <4 lanes, UDP leg not
// routing) dials one more lane; 5 minutes below the watermark drops the
// newest lane. A UDP leg in Active state suppresses adds and lets existing
// lanes age out naturally. Every add/remove is recorded verbatim in a ring
// buffer (task 2.5).
//
// Lifecycle (§5.6): lanes are data legs of the session — Close() tears down
// all of them; a single dead lane is removed (flows rehash on the next
// SendBatch) and never kills the session.

const stripeLaneQueueDepth = 256 // outbound batches per lane

// stripeAffinityCap bounds the per-flow lane affinity table; when exceeded
// the table is reset wholesale (flows re-resolve on next send, §3.6).
const stripeAffinityCap = 65536

// Tunables of the dynamic controller. Package-level vars (not consts) so
// tests can shorten the windows. All congestion quantities are
// dimensionless ratios against a sliding-min baseline (§5.4 self-normalized
// BBR shape) — no absolute time/money scales of the environment live here.
var (
	stripeCtrlTick           = 1 * time.Second  // controller decision period
	stripeProbeInterval      = 2 * time.Second  // per-lane probe ping period
	stripeProbeTimeout       = 5 * time.Second  // outstanding probe expiry (loss)
	stripeGaugeEMAAlpha      = 0.3              // sojourn EMA blend factor
	stripeGaugeDecayPerTick  = 0.5              // idle EMA pull-back rate per tick
	stripeGaugeIdleDecay     = 5 * time.Second  // no sample this long → pull-back starts
	stripeGaugeHighWatermark = 0.7              // congestion threshold
	stripeAddSustain         = 30 * time.Second // ≥watermark this long → add
	stripeAddInterval        = 10 * time.Second // minimum spacing between adds
	stripeClearSustain       = 5 * time.Minute  // <watermark this long → remove
	stripeMaxLanes           = 4                // cap on dynamic lanes
	stripeEventRingCap       = 32               // controller event ring buffer
	stripeBaselineWindow     = 10 * time.Second // sliding-min window for sojourn baseline
	stripeBaselineFloor      = time.Millisecond // noise floor for near-zero baselines
	stripeCongestMargin      = 1.0              // gauge 1.0 at sojourn = baseline × (1+margin)
	stripeWeightLossPenalty  = 2.0              // cost penalty per unit loss rate
	stripeLaneMinProbes      = uint64(8)        // degradation gate: min samples
	stripeLaneBadLoss        = 0.5              // degradation gate: loss rate
	stripeLaneBadSRTT        = time.Second      // degradation gate: srtt
)

// stripeConfig wires the dynamic controller. The zero value is a valid
// passive-side wrapper: dialLane nil means "never add lanes", udpActive nil
// means "no UDP leg to defer to".
type stripeConfig struct {
	// dialLane dials one extra lane to the same proxy and performs the
	// StripeAttach handshake, returning the assigned stripeID and the
	// established transport (the wrapper then attachLanes it). Nil on the
	// passive side.
	dialLane func() (uint32, frame.FrameTransport, error)
	// udpActive reports whether the session's UDP leg currently routes data
	// (design §7): while true the controller suppresses adds.
	udpActive func() bool
	// sendControl enqueues a control frame through the peer's control queue.
	// Used by LaneAck to report cumulative rx counts.
	sendControl func(byte, []byte) error
	// name is the peer identifier for diagnostic logs.
	name string
}

type stripeLaneProbe struct {
	mu          sync.Mutex
	nextSeq     uint32
	outstanding map[uint32]time.Time
	srtt        time.Duration // EMA of matched pong RTTs (0 = no sample yet)
	srttBase    time.Duration // sliding-min baseline of the lane's own srtt
	srttBaseAt  time.Time     // last time srttBase was (re)established
	lost        uint64        // probes that expired unmatched
	sent        uint64        // probes enqueued
	lastPongAt  time.Time
}

type stripeLane struct {
	id        uint32 // opaque nonce chosen by the initiator
	transport frame.FrameTransport
	queue     chan []frame.Frame // drained by the lane writer
	done      chan struct{}
	closeOnce sync.Once
	probe     stripeLaneProbe
	framesSent atomic.Uint64
	bytesSent  atomic.Uint64
	flow       laneFlow // in-flight accounting for this lane
	diagTx     int64    // previous tx snapshot for diag logging
}

func (l *stripeLane) close() {
	l.closeOnce.Do(func() { close(l.done) })
}

type stripeTransport struct {
	mu    sync.Mutex
	base  frame.FrameTransport // primary lane, owns the session
	lanes []*stripeLane        // dynamic extra data lanes
	salt  uint32               // own flow-hash salt, independent of DRR's
	cfg   stripeConfig

	// affinity is the per-flow sticky lane pin (mesh-per-flow-ordering §3.6):
	// a flow's first splittable frame resolves a lane (weighted rendezvous)
	// and the binding is cached here. Weight drift must NOT re-map live
	// flows — rendezvous scores multiplied by drifting probe weights made
	// flows flip-flop across parallel channels and reordered inner TCP.
	// The table is dropped wholesale on lane add/remove (accepted transient,
	// same stance as §3.3 route pinning).
	affinity map[uint32]*stripeLane

	ctrlMu           sync.Mutex // controller state (gauge + windows + events)
	sojournEMA       float64    // primary-lane sojourn EMA in ms
	sojournSamples   uint64     // samples folded into sojournEMA
	baseline         float64    // sliding-min sojourn baseline in ms (0 = none yet)
	baselineAt       time.Time  // last time baseline was (re)established
	rebaselinePending bool      // persistent-congestion escape armed (§5.4)
	gauge            float64    // congestion gauge in [0,1], derived (§5.4)
	lastSampleAt     time.Time
	highSince        time.Time // zero while gauge < watermark
	clearSince       time.Time // zero while gauge >= watermark
	lastAddAt        time.Time
	lastRemoveAt     time.Time
	events           []string // ring buffer of add/remove reasons (task 2.5)

	ctrlCh chan legRecvResult // reuses udp_leg_transport's result type
	dataCh chan legRecvResult
	closed chan struct{}
	closeOnce sync.Once

	// LaneAck metering (§3 B1)
	primaryFlow     laneFlow
	ackRequested    atomic.Bool
	ackRequestSent  atomic.Bool
	diagPrimaryTx   int64
}

func newStripeTransport(base frame.FrameTransport, cfg stripeConfig) *stripeTransport {
	t := &stripeTransport{
		base:     base,
		salt:     rand.Uint32(),
		cfg:      cfg,
		affinity: make(map[uint32]*stripeLane),
		ctrlCh:   make(chan legRecvResult, 16),
		dataCh:   make(chan legRecvResult, 64),
		closed:   make(chan struct{}),
	}
	go t.baseReadLoop()
	go t.flowLoop()
	if cfg.dialLane != nil {
		go t.controllerLoop()
	}
	return t
}

func (t *stripeTransport) String() string { return "stripe" }

// attachLane registers an established extra data lane and starts its writer
// and reader goroutines. Called by the active side after its StripeAttach
// dial and by the passive side on attaching an inbound leg (design §5.2).
func (t *stripeTransport) attachLane(id uint32, tr frame.FrameTransport) {
	l := &stripeLane{
		id:        id,
		transport: tr,
		queue:     make(chan []frame.Frame, stripeLaneQueueDepth),
		done:      make(chan struct{}),
	}
	l.probe.outstanding = make(map[uint32]time.Time)
	t.mu.Lock()
	t.lanes = append(t.lanes, l)
	t.mu.Unlock()
	// Lane set changed: every flow re-resolves on its next send (§3.6).
	t.dropAffinity()
	go t.laneWriter(l)
	go t.laneReadLoop(l)
}

// laneCount returns the number of dynamic lanes currently attached
// (primary lane excluded).
func (t *stripeTransport) laneCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.lanes)
}

func (t *stripeTransport) snapshotLanes() []*stripeLane {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]*stripeLane(nil), t.lanes...)
}

// removeLane detaches a dead lane; its affinity flows rehash across the
// remaining lanes on the next SendBatch (§5.6). Safe to call twice.
func (t *stripeTransport) removeLane(l *stripeLane) {
	t.mu.Lock()
	for i, x := range t.lanes {
		if x == l {
			t.lanes = append(t.lanes[:i], t.lanes[i+1:]...)
			break
		}
	}
	t.mu.Unlock()
	// Lane set changed: affinity flows rehash across the survivors on the
	// next SendBatch (§3.6 accepted transient).
	t.dropAffinity()
	l.close()
	l.transport.Close()
}

// --- send path ---

// splittable reports whether a frame may ride a dynamic lane: only non-DNS
// mesh packets participate in flow splitting (§5.3); control/DNS frames
// always take the primary lane.
func splittable(frameType byte, payload []byte) bool {
	return frameType == frame.FrameMeshPacket && !isDNSPacket(payload)
}

// currentWeight folds the lane's probe quality into a linkCost-like weight,
// relative to the lane's own sliding-min srtt baseline (§5.4/§六 self-normalized):
// 1/(1 + (srtt−base)/max(base,floor) + lossPenalty·loss). No probe sample or
// no baseline yet → neutral 1.0 so a fresh lane immediately attracts traffic
// (and probe evidence). Lifetime loss counters are used; phase 4.6 re-tunes
// the shape.
func (l *stripeLane) currentWeight() float64 {
	l.probe.mu.Lock()
	defer l.probe.mu.Unlock()
	if l.probe.srtt <= 0 || l.probe.srttBase <= 0 {
		return 1.0
	}
	base := l.probe.srttBase
	if base < stripeBaselineFloor {
		base = stripeBaselineFloor
	}
	rel := float64(l.probe.srtt-base) / float64(base)
	if rel < 0 {
		rel = 0 // a lane faster than its own baseline pays no srtt penalty
	}
	loss := 0.0
	if l.probe.sent > 0 {
		loss = float64(l.probe.lost) / float64(l.probe.sent)
	}
	return 1.0 / (1.0 + rel + stripeWeightLossPenalty*loss)
}

// probeSRTT returns the lane's current srtt EMA (0 = no sample yet).
func (l *stripeLane) probeSRTT() time.Duration {
	l.probe.mu.Lock()
	defer l.probe.mu.Unlock()
	return l.probe.srtt
}

// probeLossRate returns lost/sent over the lane's lifetime (0 when idle).
func (l *stripeLane) probeLossRate() float64 {
	l.probe.mu.Lock()
	defer l.probe.mu.Unlock()
	if l.probe.sent == 0 {
		return 0
	}
	return float64(l.probe.lost) / float64(l.probe.sent)
}

// scoreMix is a splitmix64 finalizer over (flow key, lane identity),
// normalized to (0,1) — deterministic per (flow, lane) pair, uniformly
// distributed, so argmax across lanes is rendezvous (highest random weight).
func scoreMix(flowKey uint32, laneID uint32) float64 {
	x := uint64(flowKey)<<32 | uint64(laneID)
	x += 0x9E3779B97F4A7C15
	x = (x ^ (x >> 30)) * 0xBF58476D1CE4E5B9
	x = (x ^ (x >> 27)) * 0x94D049BB133111EB
	x ^= x >> 31
	return (float64(x>>11) + 0.5) / float64(uint64(1)<<53)
}

// pickLane chooses the outbound lane for one splittable frame by weighted
// rendezvous (design §六): every candidate — primary (laneID 0, weight 1)
// and each dynamic lane — scores hash(flow, candidate)·weight, the argmax
// wins. Lane set changes therefore remap only the flows the changed
// candidate steals (minimal remapping, §5.6/3.4). Returns nil = primary.
//
// Only used for first-contact resolution; live flows ride the sticky
// affinity table (pickLanePinned, §3.6).
func (t *stripeTransport) pickLane(key uint32, lanes []*stripeLane) *stripeLane {
	bestScore := scoreMix(key, 0) // primary lane, weight 1.0
	var best *stripeLane
	for _, l := range lanes {
		s := scoreMix(key, l.id) * l.currentWeight()
		if s > bestScore {
			bestScore = s
			best = l
		}
	}
	return best
}

// pickLanePinned resolves the outbound lane for one splittable frame using
// the sticky affinity table (§3.6): a flow's binding survives probe-weight
// drift; only lane set changes (attach/remove → table dropped) re-map flows.
// Misses resolve via weighted rendezvous and are cached.
func (t *stripeTransport) pickLanePinned(payload []byte, lanes []*stripeLane) *stripeLane {
	key := flowHash32(t.salt, payload)
	t.mu.Lock()
	if l, ok := t.affinity[key]; ok {
		if l == nil {
			// Pinned to the primary lane — always alive.
			t.mu.Unlock()
			return nil
		}
		// Confirm the pinned lane is still attached; the table is dropped on
		// lane set changes, so a hit implies membership — but a lane removed
		// without invalidation (defensive) must fall through to re-resolve.
		stillThere := false
		for _, x := range lanes {
			if x == l {
				stillThere = true
				break
			}
		}
		if stillThere {
			t.mu.Unlock()
			return l
		}
		delete(t.affinity, key)
	}
	t.mu.Unlock()

	// Miss: weighted rendezvous (quality-aware placement for new flows).
	l := t.pickLane(key, lanes)
	if key == 0 {
		// flowHash32 returns 0 for non-IPv4/truncated payloads; those all
		// share a key, and pinning them to one lane would serialize unrelated
		// traffic — leave them unpinned (resolved per send).
		return l
	}
	t.mu.Lock()
	if len(t.affinity) >= stripeAffinityCap {
		t.affinity = make(map[uint32]*stripeLane)
	}
	if l != nil {
		t.affinity[key] = l
	} else {
		// Explicitly cache "primary" so a later weight flip cannot promote a
		// lane above the primary for this flow.
		t.affinity[key] = nil
	}
	t.mu.Unlock()
	return l
}

// dropAffinity invalidates the per-flow lane pin. Called on lane set changes.
func (t *stripeTransport) dropAffinity() {
	t.mu.Lock()
	t.affinity = make(map[uint32]*stripeLane)
	t.mu.Unlock()
}

func (t *stripeTransport) Send(frameType byte, payload []byte, isControl bool) error {
	if frameType == frame.FrameMeshPacket {
		t.requestLaneAcks()
	}
	lanes := t.snapshotLanes()
	if len(lanes) == 0 || !splittable(frameType, payload) {
		start := time.Now()
		err := t.base.Send(frameType, payload, isControl)
		t.primaryFlow.onSent(len(payload), start)
		t.sampleSojourn(start)
		return err
	}
	l := t.pickLanePinned(payload, lanes)
	if l == nil {
		start := time.Now()
		err := t.base.Send(frameType, payload, isControl)
		t.primaryFlow.onSent(len(payload), start)
		t.sampleSojourn(start)
		return err
	}
	batch := []frame.Frame{{Type: frameType, Payload: payload}}
	if ok, err := t.laneEnqueue(l, batch); ok || err != nil {
		return err
	}
	start := time.Now()
	err := t.base.Send(frameType, payload, isControl)
	t.primaryFlow.onSent(len(payload), start)
	t.sampleSojourn(start)
	return err
}

// stripeLaneFullWait bounds how long a sender waits on a full lane queue
// before moving the flows to the primary lane.
const stripeLaneFullWait = 50 * time.Millisecond

// laneEnqueue queues batch on lane l, waiting up to stripeLaneFullWait when
// the queue is full. On timeout or a dead lane it re-pins the batch's flows
// to the primary lane and returns false so the caller sends there: each flow
// changes path once instead of flapping between lane and primary (§3.9 R4).
func (t *stripeTransport) laneEnqueue(l *stripeLane, batch []frame.Frame) (bool, error) {
	select {
	case l.queue <- batch:
		return true, nil
	case <-l.done:
		t.repinPrimary(batch)
		return false, nil
	case <-t.closed:
		return false, io.ErrClosedPipe
	default:
	}
	timer := time.NewTimer(stripeLaneFullWait)
	defer timer.Stop()
	select {
	case l.queue <- batch:
		return true, nil
	case <-l.done:
	case <-timer.C:
	case <-t.closed:
		return false, io.ErrClosedPipe
	}
	t.repinPrimary(batch)
	return false, nil
}

func (t *stripeTransport) repinPrimary(batch []frame.Frame) {
	t.mu.Lock()
	for _, f := range batch {
		if key := flowHash32(t.salt, f.Payload); key != 0 {
			t.affinity[key] = nil
		}
	}
	t.mu.Unlock()
}

func (t *stripeTransport) SendBatch(frames []frame.Frame) error {
	lanes := t.snapshotLanes()
	hasMesh := false
	for _, f := range frames {
		if f.Type == frame.FrameMeshPacket {
			hasMesh = true
			break
		}
	}
	if hasMesh {
		t.requestLaneAcks()
	}
	if len(lanes) == 0 {
		start := time.Now()
		var totalBytes int
		for _, f := range frames {
			totalBytes += len(f.Payload)
		}
		err := t.base.SendBatch(frames)
		t.primaryFlow.onSent(totalBytes, start)
		t.sampleSojourn(start)
		return err
	}
	var primary []frame.Frame
	perLane := make(map[*stripeLane][]frame.Frame)
	for _, f := range frames {
		if !splittable(f.Type, f.Payload) {
			primary = append(primary, f)
			continue
		}
		l := t.pickLanePinned(f.Payload, lanes)
		if l == nil {
			primary = append(primary, f)
			continue
		}
		perLane[l] = append(perLane[l], f)
	}
	var firstErr error
	if len(primary) > 0 {
		start := time.Now()
		var primaryBytes int
		for _, f := range primary {
			primaryBytes += len(f.Payload)
		}
		firstErr = t.base.SendBatch(primary)
		t.primaryFlow.onSent(primaryBytes, start)
		t.sampleSojourn(start)
	}
	for l, batch := range perLane {
		ok, err := t.laneEnqueue(l, batch)
		if err != nil {
			return t.firstErr(firstErr, err)
		}
		if !ok {
			start := time.Now()
			var fb int
			for _, f := range batch {
				fb += len(f.Payload)
			}
			firstErr = t.firstErr(firstErr, t.base.SendBatch(batch))
			t.primaryFlow.onSent(fb, start)
			t.sampleSojourn(start)
		}
	}
	return firstErr
}

func (t *stripeTransport) firstErr(cur, next error) error {
	if cur != nil {
		return cur
	}
	return next
}

// laneWriter drains one lane's outbound queue. A write failure kills the
// lane (the session and remaining lanes keep going).
func (t *stripeTransport) laneWriter(l *stripeLane) {
	for {
		select {
		case <-l.done:
			return
		case <-t.closed:
			return
		case batch := <-l.queue:
			var totalBytes int
			for _, f := range batch {
				totalBytes += len(f.Payload)
			}
			if err := l.transport.SendBatch(batch); err != nil {
				util.LogModuleWarn("p2p", "[STRIPE] lane %d write failed, removing lane: %v", l.id, err)
				t.removeLane(l)
				return
			}
			l.framesSent.Add(uint64(len(batch)))
			l.bytesSent.Add(uint64(totalBytes))
			l.flow.onSent(totalBytes, time.Now())
		}
	}
}

// --- recv path ---

// baseReadLoop pumps the primary lane into the shared channels, preserving
// control priority (udp_leg_transport.baseReadLoop pattern).
func (t *stripeTransport) baseReadLoop() {
	for {
		ft, payload, err := t.base.Recv()
		if err != nil {
			select {
			case t.ctrlCh <- legRecvResult{err: err}:
			case <-t.closed:
			}
			return
		}
		t.primaryFlow.onRecv(len(payload))
		if ft == frame.FrameLaneAck {
			t.handleLaneAck(payload)
			continue
		}
		ch := t.dataCh
		if ft != frame.FrameMeshPacket {
			ch = t.ctrlCh
		}
		select {
		case ch <- legRecvResult{frameType: ft, payload: payload}:
		case <-t.closed:
			return
		}
	}
}

// laneReadLoop pumps one lane into the shared channels. Lanes accept only
// mesh data and link probes; a control/gossip frame on a lane is a protocol
// error (warn + drop, §5.5). A read error kills only the lane.
func (t *stripeTransport) laneReadLoop(l *stripeLane) {
	for {
		ft, payload, err := l.transport.Recv()
		if err != nil {
			select {
			case <-t.closed:
			default:
				util.LogModuleDebug("p2p", "[STRIPE] lane %d read ended, removing lane: %v", l.id, err)
				t.removeLane(l)
			}
			return
		}
		l.flow.onRecv(len(payload))
		switch ft {
		case frame.FrameMeshPacket:
			select {
			case t.dataCh <- legRecvResult{frameType: ft, payload: payload}:
			case <-t.closed:
				return
			}
		case frame.FrameLinkPing:
			// Lane-internal probe (task 2.1): answer on the same lane,
			// never surface to the session.
			t.replyLanePing(l, payload)
		case frame.FrameLinkPong:
			t.handleLanePong(l, payload)
		default:
			util.LogModuleWarn("p2p", "[STRIPE] control frame 0x%02x on lane %d, dropping", ft, l.id)
			frame.ReturnBuffer(payload)
		}
	}
}

// replyLanePing answers a lane probe with a pong on the same lane, echoing
// timestamp and seq (PingInfo wire format, same as session-level probes —
// old code paths treat it uniformly).
func (t *stripeTransport) replyLanePing(l *stripeLane, payload []byte) {
	var ping PingInfo
	if err := json.Unmarshal(payload, &ping); err != nil {
		util.LogModuleDebug("p2p", "[STRIPE] lane %d invalid probe ping: %v", l.id, err)
		frame.ReturnBuffer(payload)
		return
	}
	frame.ReturnBuffer(payload)
	data, err := json.Marshal(PingInfo{Timestamp: ping.Timestamp, Seq: ping.Seq})
	if err != nil {
		return
	}
	select {
	case l.queue <- []frame.Frame{{Type: frame.FrameLinkPong, Payload: data}}:
	case <-l.done:
	case <-t.closed:
	default:
		// Probe reply yields to data; the prober counts the lost pong.
	}
}

// handleLanePong matches a pong against this lane's outstanding probes and
// folds the RTT into the lane's srtt EMA. Unmatched pongs are dropped.
func (t *stripeTransport) handleLanePong(l *stripeLane, payload []byte) {
	var pong PingInfo
	if err := json.Unmarshal(payload, &pong); err != nil {
		frame.ReturnBuffer(payload)
		return
	}
	frame.ReturnBuffer(payload)
	l.probe.mu.Lock()
	if at, ok := l.probe.outstanding[pong.Seq]; ok {
		rtt := time.Since(at)
		if rtt <= 0 {
			// Windows clock granularity can round a same-tick round trip to
			// zero; keep the sample so srtt stays meaningful.
			rtt = time.Nanosecond
		}
		if l.probe.srtt == 0 {
			l.probe.srtt = rtt
		} else {
			l.probe.srtt += (rtt - l.probe.srtt) / 8
		}
		// Sliding-min baseline of the lane's own srtt (BBR RTprop shape,
		// §5.4): a lane is only "slow" relative to how fast it used to be.
		now := time.Now()
		if l.probe.srttBase == 0 || rtt < l.probe.srttBase || now.Sub(l.probe.srttBaseAt) >= stripeBaselineWindow {
			l.probe.srttBase = rtt
			l.probe.srttBaseAt = now
		}
		l.probe.lastPongAt = now
		delete(l.probe.outstanding, pong.Seq)
	}
	l.probe.mu.Unlock()
}

func (t *stripeTransport) Recv() (byte, []byte, error) {
	// Priority: non-blocking check for control frames
	select {
	case r := <-t.ctrlCh:
		return r.frameType, r.payload, r.err
	default:
	}
	select {
	case r := <-t.ctrlCh:
		return r.frameType, r.payload, r.err
	case r := <-t.dataCh:
		return r.frameType, r.payload, r.err
	case <-t.closed:
		return 0, nil, io.ErrClosedPipe
	}
}

// --- dynamic controller (tasks 2.x) ---

// sampleSojourn folds one primary-lane send duration into the sojourn EMA
// and refreshes the sliding-min baseline, then derives the congestion gauge
// (§5.4 self-normalized, BBR RTprop shape):
//
//	baseline = sliding min of sojourn over stripeBaselineWindow
//	gauge    = clamp((sojournEMA − baseline) / (max(baseline, floor) × margin), 0, 1)
//
// Congestion only ever pushes sojourn up, so the window's minimum is the
// "uncongested" signature. Absolute ms values never cross environments —
// only the dimensionless margin and watermarks are tuned (phase 4.6).
func (t *stripeTransport) sampleSojourn(start time.Time) {
	if start.IsZero() {
		return
	}
	ms := float64(time.Since(start)) / float64(time.Millisecond)
	now := time.Now()
	t.ctrlMu.Lock()
	if t.sojournSamples == 0 {
		t.sojournEMA = ms
	} else {
		t.sojournEMA += stripeGaugeEMAAlpha * (ms - t.sojournEMA)
	}
	t.sojournSamples++
	if t.baseline == 0 || ms < t.baseline || now.Sub(t.baselineAt) >= stripeBaselineWindow {
		// Window expired: if the gauge has been pinned near max the whole
		// window, congestion is the new normal — escape by re-basing on the
		// current EMA instead of a single (possibly still-congested) sample,
		// and flag the event so the re-baseline is visible in the ring.
		floorMs := float64(stripeBaselineFloor) / float64(time.Millisecond)
		if t.baseline > 0 && now.Sub(t.baselineAt) >= stripeBaselineWindow && t.gauge >= 0.95 {
			t.baseline = t.sojournEMA / (1 + stripeCongestMargin*0.95)
			if t.baseline < floorMs {
				t.baseline = floorMs
			}
			t.rebaselinePending = true
		} else {
			// Clamp to the noise floor: a same-tick sample (ms == 0, common
			// on Windows' coarse clock) must not leave baseline == 0, or the
			// "baseline == 0 means unbootstrapped" sentinel breaks and the
			// next congested sample re-bases the line at congestion level.
			t.baseline = ms
			if t.baseline < floorMs {
				t.baseline = floorMs
			}
		}
		t.baselineAt = now
	}
	t.gauge = t.computeGaugeLocked()
	t.lastSampleAt = now
	t.ctrlMu.Unlock()
}

// computeGaugeLocked derives gauge from sojournEMA and baseline (§5.4).
func (t *stripeTransport) computeGaugeLocked() float64 {
	if t.baseline <= 0 || t.sojournSamples == 0 {
		return 0
	}
	base := t.baseline
	if base < float64(stripeBaselineFloor)/float64(time.Millisecond) {
		base = float64(stripeBaselineFloor) / float64(time.Millisecond)
	}
	g := (t.sojournEMA - t.baseline) / (base * stripeCongestMargin)
	if g < 0 {
		g = 0
	}
	if g > 1 {
		g = 1
	}
	return g
}

// controllerLoop drives the add/remove decisions and the per-lane probes.
func (t *stripeTransport) controllerLoop() {
	tick := time.NewTicker(stripeCtrlTick)
	probe := time.NewTicker(stripeProbeInterval)
	defer tick.Stop()
	defer probe.Stop()
	for {
		select {
		case <-t.closed:
			return
		case <-tick.C:
			t.controllerTick()
		case <-probe.C:
			t.probeTick()
		}
	}
}

func (t *stripeTransport) controllerTick() {
	now := time.Now()

	t.ctrlMu.Lock()
	// Idle decay: with no fresh samples for a full idle window the sojourn
	// EMA is pulled back toward the baseline (not multiplied toward zero —
	// the baseline, not 0, is the "uncongested" anchor, §5.4), so a quiet
	// session reads as clear (and idle lanes age out, §2.3). Active senders
	// keep refreshing lastSampleAt and are never decayed mid-send.
	if !t.lastSampleAt.IsZero() && now.Sub(t.lastSampleAt) >= stripeGaugeIdleDecay {
		t.sojournEMA += (t.baseline - t.sojournEMA) * stripeGaugeDecayPerTick
		t.gauge = t.computeGaugeLocked()
	}
	high := t.gauge >= stripeGaugeHighWatermark
	switch {
	case high:
		if t.highSince.IsZero() {
			t.highSince = now
		}
		t.clearSince = time.Time{}
	default:
		t.highSince = time.Time{}
		if t.clearSince.IsZero() {
			t.clearSince = now
		}
	}
	udpActive := t.cfg.udpActive != nil && t.cfg.udpActive()
	if udpActive {
		// UDP leg carries the data (design §7): no new lanes while Active.
		t.highSince = time.Time{}
	}
	addDue := !t.highSince.IsZero() &&
		now.Sub(t.highSince) >= stripeAddSustain &&
		now.Sub(t.lastAddAt) >= stripeAddInterval &&
		t.laneCount() < stripeMaxLanes
	removeDue := !udpActive &&
		t.laneCount() > 0 &&
		!t.clearSince.IsZero() &&
		now.Sub(t.clearSince) >= stripeClearSustain
	if addDue {
		t.lastAddAt = now
	}
	if removeDue {
		t.lastRemoveAt = now
	}
	t.ctrlMu.Unlock()

	t.ctrlMu.Lock()
	rebaseline := t.rebaselinePending
	t.rebaselinePending = false
	t.ctrlMu.Unlock()
	if rebaseline {
		t.recordEvent("re-baseline: persistent congestion became the new normal (gauge pinned ≥0.95 for %v), baseline rebuilt from current EMA", stripeBaselineWindow)
	}

	if addDue {
		stripeID, tr, err := t.cfg.dialLane()
		if err != nil || tr == nil {
			if err != nil {
				util.LogModuleInfo("p2p", "[STRIPE] controller lane dial failed: %v", err)
			}
		} else {
			t.attachLane(stripeID, tr)
			t.recordEvent("add lane stripeID=%d: gauge=%.2f ≥ %.2f sustained %v, lanes now %d/%d (udpActive=%v)",
				stripeID, t.currentGauge(), stripeGaugeHighWatermark, stripeAddSustain, t.laneCount(), stripeMaxLanes, udpActive)
		}
	}
	if removeDue {
		lanes := t.snapshotLanes()
		if len(lanes) > 0 {
			victim := lanes[len(lanes)-1] // newest first (§2.3)
			t.removeLane(victim)
			t.recordEvent("remove lane stripeID=%d: gauge < %.2f sustained %v (udpActive=%v)",
				victim.id, stripeGaugeHighWatermark, stripeClearSustain, udpActive)
		}
	}

	// Degradation invalidation (§3.3): a lane whose probes show sustained
	// atrocious quality is removed proactively; its affinity flows rehash
	// across the survivors on the next SendBatch.
	for _, l := range t.snapshotLanes() {
		l.probe.mu.Lock()
		sent := l.probe.sent
		srtt := l.probe.srtt
		loss := 0.0
		if sent > 0 {
			loss = float64(l.probe.lost) / float64(sent)
		}
		l.probe.mu.Unlock()
		if sent >= stripeLaneMinProbes && (loss >= stripeLaneBadLoss || (srtt > 0 && srtt >= stripeLaneBadSRTT)) {
			t.recordEvent("remove lane stripeID=%d: degraded (srtt=%v loss=%.0f%% over %d probes), affinity flows rehash",
				l.id, srtt, loss*100, sent)
			t.removeLane(l)
		}
	}
}

// probeTick sends one PingInfo probe per lane and ages out expired ones as
// losses. Probes ride the lane queue (never write the lane transport
// directly — the lane writer is the only writer, §1.5).
func (t *stripeTransport) probeTick() {
	now := time.Now()
	for _, l := range t.snapshotLanes() {
		l.probe.mu.Lock()
		for seq, at := range l.probe.outstanding {
			if now.Sub(at) > stripeProbeTimeout {
				delete(l.probe.outstanding, seq)
				l.probe.lost++
			}
		}
		l.probe.nextSeq++
		seq := l.probe.nextSeq
		l.probe.outstanding[seq] = now
		l.probe.sent++
		l.probe.mu.Unlock()

		data, err := json.Marshal(PingInfo{Timestamp: now.UnixNano(), Seq: seq})
		if err != nil {
			continue
		}
		select {
		case l.queue <- []frame.Frame{{Type: frame.FrameLinkPing, Payload: data}}:
		case <-l.done:
		case <-t.closed:
		default:
			// Queue full: skip this probe and un-count it.
			l.probe.mu.Lock()
			delete(l.probe.outstanding, seq)
			l.probe.sent--
			l.probe.mu.Unlock()
		}
	}
}

func (t *stripeTransport) currentGauge() float64 {
	t.ctrlMu.Lock()
	defer t.ctrlMu.Unlock()
	return t.gauge
}

// snapshotEvents returns the controller event ring buffer (oldest first).
func (t *stripeTransport) snapshotEvents() []string {
	t.ctrlMu.Lock()
	defer t.ctrlMu.Unlock()
	return append([]string(nil), t.events...)
}

// recordEvent appends one verbatim controller reason to the ring buffer and
// mirrors it to the log (task 2.5).
func (t *stripeTransport) recordEvent(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	util.LogModuleInfo("p2p", "[STRIPE] %s", msg)
	t.ctrlMu.Lock()
	t.events = append(t.events, msg)
	if len(t.events) > stripeEventRingCap {
		copy(t.events, t.events[len(t.events)-stripeEventRingCap:])
		t.events = t.events[:stripeEventRingCap]
	}
	t.ctrlMu.Unlock()
}

// --- introspection for admin ---

// transportForm returns the admin badge value (design §7): "tcp" with no
// dynamic lanes, "tcp+stripe×N" once lanes attach.
func (t *stripeTransport) transportForm() string {
	n := t.laneCount()
	if n == 0 {
		return "tcp"
	}
	return fmt.Sprintf("tcp+stripe×%d", n)
}

// snapshotInfo returns per-lane observability rows for the admin API
// (design §7). Rendezvous is stateless — there is no affinity table to
// size, lanes carry their own flow population implicitly.
func (t *stripeTransport) snapshotInfo() []StripeInfo {
	lanes := t.snapshotLanes()
	infos := make([]StripeInfo, 0, len(lanes))
	for _, l := range lanes {
		l.probe.mu.Lock()
		srtt := l.probe.srtt
		sent, lost := l.probe.sent, l.probe.lost
		l.probe.mu.Unlock()
		loss := 0.0
		if sent > 0 {
			loss = float64(lost) / float64(sent)
		}
		infos = append(infos, StripeInfo{
			LaneID:     l.id,
			SRTTMs:     float64(srtt) / float64(time.Millisecond),
			LossRate:   loss,
			Weight:     l.currentWeight(),
			FramesSent: l.framesSent.Load(),
			BytesSent:  l.bytesSent.Load(),
		})
	}
	return infos
}

func (t *stripeTransport) Close() error {
	t.closeOnce.Do(func() {
		close(t.closed)
		for _, l := range t.snapshotLanes() {
			l.close()
			l.transport.Close()
		}
		t.mu.Lock()
		t.lanes = nil
		t.mu.Unlock()
		t.base.Close()
	})
	return nil
}
