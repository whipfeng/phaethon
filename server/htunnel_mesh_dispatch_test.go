package server

import (
	"encoding/binary"
	"testing"
	"time"

	"phaethon/config"
	"phaethon/frame"
)

func newTestWaiter() *meshWaiter {
	return &meshWaiter{batch: make(chan []meshMsg, 1), done: make(chan struct{})}
}

func dataMsg(seq int, size int) meshMsg {
	p := make([]byte, size)
	binary.BigEndian.PutUint32(p, uint32(seq))
	return meshMsg{frameType: frame.FrameMeshPacket, payload: p}
}

func takeBatch(t *testing.T, w *meshWaiter) []meshMsg {
	t.Helper()
	select {
	case b := <-w.batch:
		return b
	default:
		t.Fatal("waiter got no batch")
		return nil
	}
}

// A pong queued behind a large data backlog must be served by the very next
// GET (§3.8: data used to starve control frames).
func TestMeshDispatch_ControlFirst(t *testing.T) {
	ch := &htChannel{}
	for i := 0; i < 1000; i++ {
		ch.routeFrameLocked(dataMsg(i, 1000))
	}
	ch.routeFrameLocked(meshMsg{frameType: frame.FrameLinkPong, payload: []byte("pong"), control: true})

	w := newTestWaiter()
	ch.addWaiterLocked(w)
	ch.flushLocked(64 * 1024)

	b := takeBatch(t, w)
	if !b[0].control || b[0].frameType != frame.FrameLinkPong {
		t.Fatalf("first frame = %+v, want pong", b[0])
	}
	if len(ch.ctrlQ) != 0 {
		t.Fatalf("ctrlQ not drained: %d", len(ch.ctrlQ))
	}
}

func TestMeshDispatch_ByteBatchAndOrder(t *testing.T) {
	ch := &htChannel{}
	const n, size, limit = 200, 10_000, 100_000
	for i := 0; i < n; i++ {
		ch.routeFrameLocked(dataMsg(i, size))
	}

	next := 0
	for next < n {
		w := newTestWaiter()
		ch.addWaiterLocked(w)
		ch.flushLocked(limit)
		b := takeBatch(t, w)
		bytes := 0
		for _, m := range b {
			if got := int(binary.BigEndian.Uint32(m.payload)); got != next {
				t.Fatalf("frame seq %d, want %d", got, next)
			}
			next++
			bytes += m.wireSize()
		}
		if bytes > limit {
			t.Fatalf("batch %d bytes exceeds limit %d", bytes, limit)
		}
		if len(b) != limit/(size+3) && next != n {
			t.Fatalf("batch has %d frames, want %d", len(b), limit/(size+3))
		}
	}
	if ch.dataQ != nil {
		t.Fatal("drained data queue should be nil")
	}
}

func TestMeshDispatch_OversizedFrameStillShips(t *testing.T) {
	ch := &htChannel{}
	ch.routeFrameLocked(dataMsg(0, 4096))
	w := newTestWaiter()
	ch.addWaiterLocked(w)
	ch.flushLocked(1024)
	if b := takeBatch(t, w); len(b) != 1 {
		t.Fatalf("got %d frames, want 1", len(b))
	}
}

// Pings are data-path frames: they queue in the data FIFO in arrival order.
func TestMeshDispatch_PingsRideDataFIFO(t *testing.T) {
	ch := &htChannel{}
	ch.routeFrameLocked(dataMsg(0, 100))
	ch.routeFrameLocked(meshMsg{frameType: frame.FrameLinkPing, payload: []byte("ping")})
	ch.routeFrameLocked(dataMsg(1, 100))
	if len(ch.ctrlQ) != 0 || len(ch.dataQ) != 3 {
		t.Fatalf("ctrl=%d data=%d, want 0/3", len(ch.ctrlQ), len(ch.dataQ))
	}
	w := newTestWaiter()
	ch.addWaiterLocked(w)
	ch.flushLocked(64 * 1024)
	b := takeBatch(t, w)
	if len(b) != 3 || b[1].frameType != frame.FrameLinkPing {
		t.Fatalf("batch order broken: %+v", b)
	}
}

func TestMeshDispatch_StaleWaiterSkipped(t *testing.T) {
	ch := &htChannel{}
	stale := newTestWaiter()
	fresh := newTestWaiter()
	ch.addWaiterLocked(stale)
	ch.addWaiterLocked(fresh)
	if b := ch.abandonWaiter(stale); b != nil {
		t.Fatal("abandoned idle waiter returned a batch")
	}
	ch.routeFrameLocked(dataMsg(0, 100))
	ch.flushLocked(64 * 1024)
	takeBatch(t, fresh)
	if len(stale.batch) != 0 {
		t.Fatal("stale waiter received a batch")
	}
}

// A batch handed off just before the handler times out must still be served.
func TestMeshDispatch_AbandonKeepsHandedOffBatch(t *testing.T) {
	ch := &htChannel{}
	w := newTestWaiter()
	ch.addWaiterLocked(w)
	ch.routeFrameLocked(dataMsg(7, 100))
	ch.flushLocked(64 * 1024)
	b := ch.abandonWaiter(w)
	if len(b) != 1 || binary.BigEndian.Uint32(b[0].payload) != 7 {
		t.Fatalf("abandon lost handed-off batch: %+v", b)
	}
}

func TestCodel_NoDropBelowTarget(t *testing.T) {
	var c codel
	for now := time.Second; now < 5*time.Second; now += time.Millisecond {
		if c.shouldDrop(codelTarget-time.Millisecond, now, 100) {
			t.Fatalf("dropped below target at %v", now)
		}
	}
}

func TestCodel_DropsStandingQueueSparsely(t *testing.T) {
	var c codel
	drops := 0
	start := time.Second
	for now := start; now < start+3*time.Second; now += time.Millisecond {
		if c.shouldDrop(200*time.Millisecond, now, 100) {
			if drops == 0 && now-start < codelInterval {
				t.Fatalf("first drop at %v, before one interval", now-start)
			}
			drops++
		}
	}
	// ~3s of standing queue: control law gives a handful of drops, not a flood.
	if drops < 3 || drops > 40 {
		t.Fatalf("drops = %d, want sparse (3..40)", drops)
	}
}

func TestCodel_NeverDropsNearlyEmptyQueue(t *testing.T) {
	var c codel
	for now := time.Second; now < 3*time.Second; now += time.Millisecond {
		if c.shouldDrop(time.Second, now, 1) {
			t.Fatal("dropped with backlog < 2")
		}
	}
}

// Stale frames are dropped by AQM but never pings, and the batch is never empty.
func TestMeshDispatch_AQMDropsOnlyMeshPackets(t *testing.T) {
	ch := &htChannel{}
	for i := 0; i < 50; i++ {
		ch.routeFrameLocked(dataMsg(i, 100))
		ch.routeFrameLocked(meshMsg{frameType: frame.FrameLinkPing, payload: []byte("ping")})
	}
	for i := range ch.dataQ {
		ch.dataQ[i].enq = meshNow() - time.Second // sojourn far above target
	}
	ch.dataCodel = codel{firstAbove: meshNow() - time.Second} // above target for over an interval
	w := newTestWaiter()
	ch.addWaiterLocked(w)
	ch.flushLocked(1 << 20)
	b := takeBatch(t, w)
	pings := 0
	for _, m := range b {
		if m.frameType == frame.FrameLinkPing {
			pings++
		}
	}
	if pings != 50 {
		t.Fatalf("pings delivered = %d, want 50", pings)
	}
	if ch.codelDrops == 0 || len(b)+int(ch.codelDrops) != 100 {
		t.Fatalf("delivered %d + dropped %d, want sum 100 with drops > 0", len(b), ch.codelDrops)
	}
	if ch.dataBytes != 0 {
		t.Fatalf("dataBytes = %d after full drain", ch.dataBytes)
	}
}

func TestMeshDispatch_DataBytesAccounting(t *testing.T) {
	ch := &htChannel{}
	ch.routeFrameLocked(dataMsg(0, 1000))
	ch.routeFrameLocked(dataMsg(1, 500))
	ch.routeFrameLocked(meshMsg{frameType: frame.FrameLinkPong, payload: []byte("pong"), control: true})
	if want := 1003 + 503; ch.dataBytes != want {
		t.Fatalf("dataBytes = %d, want %d (control excluded)", ch.dataBytes, want)
	}
	w := newTestWaiter()
	ch.addWaiterLocked(w)
	ch.flushLocked(1010)
	takeBatch(t, w)
	if ch.dataBytes != 503 {
		t.Fatalf("dataBytes = %d after first frame drained, want 503", ch.dataBytes)
	}
}

// Past the byte cap the dispatcher stops reading data but control frames
// still reach a GET.
func TestMeshDispatchLoop_CapPausesDataNotControl(t *testing.T) {
	ch := &htChannel{
		closed:      make(chan struct{}),
		meshOut:     make(chan meshMsg, 4),
		meshCtrlOut: make(chan meshMsg, 4),
		getWaiters:  make(chan *meshWaiter, 4),
	}
	ch.dataBytes = htMeshMaxQueuedBytes
	s := &HTunnelServer{BaseServer: BaseServer{Mapping: &config.Mapping{Name: "t"}}}
	go s.meshDispatchLoop(ch)
	defer close(ch.closed)

	ch.meshOut <- dataMsg(0, 10)
	ch.meshCtrlOut <- meshMsg{frameType: frame.FrameLinkPong, payload: []byte("pong"), control: true}
	w := newTestWaiter()
	ch.getWaiters <- w
	select {
	case b := <-w.batch:
		if len(b) != 1 || !b[0].control {
			t.Fatalf("batch = %+v, want only the pong", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("control frame not delivered while data paused")
	}
	if len(ch.meshOut) != 1 {
		t.Fatalf("meshOut len = %d, data should stay unread past the cap", len(ch.meshOut))
	}
}

// A batch keeps per-frame control semantics: control frames take the
// control path (served first by GET), the rest ride the data path.
func TestMeshChannelSendBatch_ControlSplit(t *testing.T) {
	ch := &htChannel{
		closed:      make(chan struct{}),
		meshOut:     make(chan meshMsg, 4),
		meshCtrlOut: make(chan meshMsg, 4),
	}
	tr := meshChannelTransport{ch: ch}
	err := tr.SendBatch([]frame.Frame{
		{Type: frame.FrameLinkPong, Payload: []byte("pong"), Control: true},
		{Type: frame.FrameMeshPacket, Payload: []byte("data")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ch.meshCtrlOut) != 1 || len(ch.meshOut) != 1 {
		t.Fatalf("ctrl=%d data=%d, want 1/1", len(ch.meshCtrlOut), len(ch.meshOut))
	}
	if m := <-ch.meshCtrlOut; !m.control || m.frameType != frame.FrameLinkPong {
		t.Fatalf("control frame = %+v", m)
	}
}
