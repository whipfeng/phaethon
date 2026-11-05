package p2p

import (
	"fmt"
	"sync"

	"phaethon/frame"
)

// stripeTransport overlays dynamic extra data lanes (stripes) on the primary
// proxy connection within a single P2P session
// (docs/plans/mesh-multilink-stripe-design.md §5). It presents one
// FrameTransport to the peer layer — lanes are invisible to routing, gossip
// and DRR; splitting happens inside SendBatch, fan-in inside Recv.
//
// Skeleton stage (tasks 1.2): a single primary lane. Send/SendBatch/Recv are
// pure pass-through, byte-for-byte identical to the unwrapped transport.
// Lane machinery (StripeAttach dial/attach, per-lane write/read, fan-in)
// lands in tasks 1.3-1.6.

type stripeLane struct {
	id        uint32               // opaque nonce chosen by the initiator
	transport frame.FrameTransport // owns the lane connection
}

type stripeTransport struct {
	mu    sync.Mutex
	base  frame.FrameTransport // primary lane, owns the session
	lanes []*stripeLane        // extra data lanes (empty at skeleton stage)

	closed    chan struct{}
	closeOnce sync.Once
}

func newStripeTransport(base frame.FrameTransport) *stripeTransport {
	return &stripeTransport{
		base:   base,
		closed: make(chan struct{}),
	}
}

func (t *stripeTransport) String() string { return "stripe" }

// attachLane registers an established extra data lane (tasks 1.3/1.4 wire the
// StripeAttach handshake that produces these). Until the dispatch/fan-in
// machinery lands (tasks 1.5/1.6) attached lanes carry no traffic and start
// no read loop.
func (t *stripeTransport) attachLane(id uint32, tr frame.FrameTransport) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lanes = append(t.lanes, &stripeLane{id: id, transport: tr})
}

// laneCount returns the number of dynamic lanes currently attached
// (primary lane excluded).
func (t *stripeTransport) laneCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.lanes)
}

// --- send path ---

func (t *stripeTransport) Send(frameType byte, payload []byte, isControl bool) error {
	// Control/DNS frames always ride the primary lane (design §5.3); with no
	// dynamic lanes every frame does — exactly the unwrapped behavior.
	return t.base.Send(frameType, payload, isControl)
}

func (t *stripeTransport) SendBatch(frames []frame.Frame) error {
	// 1 lane (skeleton): one writev through the base transport. When dynamic
	// lanes attach (tasks 1.5), frames split by inner 5-tuple across lanes here.
	return t.base.SendBatch(frames)
}

// --- recv path ---

func (t *stripeTransport) Recv() (byte, []byte, error) {
	// 1 lane (skeleton): straight from the primary. Lane fan-in arrives in 1.6.
	return t.base.Recv()
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
		t.mu.Lock()
		lanes := t.lanes
		t.lanes = nil
		t.mu.Unlock()
		// Lanes are data legs of the session: primary disconnect tears down
		// all of them (design §5.6).
		for _, l := range lanes {
			l.transport.Close()
		}
		t.base.Close()
	})
	return nil
}
