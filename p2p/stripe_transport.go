package p2p

import (
	"fmt"
	"io"
	"math/rand"
	"sync"

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
//
// Lifecycle (§5.6): lanes are data legs of the session — Close() tears down
// all of them; a single dead lane is removed (flows rehash on the next
// SendBatch) and never kills the session.

const stripeLaneQueueDepth = 256 // outbound batches per lane

type stripeLane struct {
	id        uint32 // opaque nonce chosen by the initiator
	transport frame.FrameTransport
	queue     chan []frame.Frame // drained by the lane writer
	done      chan struct{}
	closeOnce sync.Once
}

func (l *stripeLane) close() {
	l.closeOnce.Do(func() { close(l.done) })
}

type stripeTransport struct {
	mu    sync.Mutex
	base  frame.FrameTransport // primary lane, owns the session
	lanes []*stripeLane        // dynamic extra data lanes
	salt  uint32               // own flow-hash salt, independent of DRR's

	ctrlCh chan legRecvResult // reuses udp_leg_transport's result type
	dataCh chan legRecvResult
	closed chan struct{}
	closeOnce sync.Once
}

func newStripeTransport(base frame.FrameTransport) *stripeTransport {
	t := &stripeTransport{
		base:   base,
		salt:   rand.Uint32(),
		ctrlCh: make(chan legRecvResult, 16),
		dataCh: make(chan legRecvResult, 64),
		closed: make(chan struct{}),
	}
	go t.baseReadLoop()
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
	t.mu.Lock()
	t.lanes = append(t.lanes, l)
	t.mu.Unlock()
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

func (t *stripeTransport) Send(frameType byte, payload []byte, isControl bool) error {
	lanes := t.snapshotLanes()
	if len(lanes) == 0 || !splittable(frameType, payload) {
		return t.base.Send(frameType, payload, isControl)
	}
	idx := int(hashFlowWithSalt(t.salt, payload)) % (len(lanes) + 1)
	if idx == 0 {
		return t.base.Send(frameType, payload, isControl)
	}
	l := lanes[idx-1]
	select {
	case l.queue <- []frame.Frame{{Type: frameType, Payload: payload}}:
		return nil
	case <-l.done:
	case <-t.closed:
		return io.ErrClosedPipe
	default:
	}
	// Lane dead/closing or queue full: fall back to the primary lane.
	return t.base.Send(frameType, payload, isControl)
}

func (t *stripeTransport) SendBatch(frames []frame.Frame) error {
	lanes := t.snapshotLanes()
	if len(lanes) == 0 {
		return t.base.SendBatch(frames)
	}
	var primary []frame.Frame
	perLane := make(map[*stripeLane][]frame.Frame)
	for _, f := range frames {
		if !splittable(f.Type, f.Payload) {
			primary = append(primary, f)
			continue
		}
		idx := int(hashFlowWithSalt(t.salt, f.Payload)) % (len(lanes) + 1)
		if idx == 0 {
			primary = append(primary, f)
			continue
		}
		l := lanes[idx-1]
		perLane[l] = append(perLane[l], f)
	}
	var firstErr error
	if len(primary) > 0 {
		firstErr = t.base.SendBatch(primary)
	}
	for l, batch := range perLane {
		select {
		case l.queue <- batch:
		case <-l.done:
			firstErr = t.firstErr(firstErr, t.base.SendBatch(batch))
		case <-t.closed:
			return t.firstErr(firstErr, io.ErrClosedPipe)
		default:
			// Queue full: bounded memory beats unbounded backpressure here;
			// the overflow rides the primary lane instead of dropping.
			firstErr = t.firstErr(firstErr, t.base.SendBatch(batch))
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
			if err := l.transport.SendBatch(batch); err != nil {
				util.LogModuleWarn("p2p", "[STRIPE] lane %d write failed, removing lane: %v", l.id, err)
				t.removeLane(l)
				return
			}
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
		switch ft {
		case frame.FrameMeshPacket:
			select {
			case t.dataCh <- legRecvResult{frameType: ft, payload: payload}:
			case <-t.closed:
				return
			}
		case frame.FrameLinkPing, frame.FrameLinkPong:
			select {
			case t.ctrlCh <- legRecvResult{frameType: ft, payload: payload}:
			case <-t.closed:
				return
			}
		default:
			util.LogModuleWarn("p2p", "[STRIPE] control frame 0x%02x on lane %d, dropping", ft, l.id)
			frame.ReturnBuffer(payload)
		}
	}
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
