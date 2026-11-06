package server

import (
	"encoding/binary"
	"testing"

	"phaethon/frame"
)

func newDispatchTestChannel(pipes int) *htChannel {
	ch := &htChannel{pipeCnt: pipes}
	ch.pipeQ = make([][]meshMsg, pipes)
	ch.pipeWait = make([][]*meshWaiter, pipes)
	return ch
}

func newTestWaiter(pipe int) *meshWaiter {
	return &meshWaiter{batch: make(chan []meshMsg, 1), done: make(chan struct{}), pipe: pipe}
}

func dataMsg(flowKey uint32, seq int, size int) meshMsg {
	p := make([]byte, size)
	binary.BigEndian.PutUint32(p, uint32(seq))
	return meshMsg{frameType: frame.FrameMeshPacket, payload: p, flowKey: flowKey}
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

// A pong queued behind a large data backlog on every pipe must be served by
// the very next GET (§3.8: pipe data used to starve control frames).
func TestMeshDispatch_ControlFirst(t *testing.T) {
	ch := newDispatchTestChannel(2)
	for i := 0; i < 1000; i++ {
		ch.routeFrameLocked(dataMsg(uint32(i%2), i, 1000))
	}
	ch.routeFrameLocked(meshMsg{frameType: frame.FrameLinkPong, payload: []byte("pong"), flowKey: flowKeyAny, control: true})

	w := newTestWaiter(1)
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
	ch := newDispatchTestChannel(2)
	const n, size, limit = 200, 10_000, 100_000
	for i := 0; i < n; i++ {
		ch.routeFrameLocked(dataMsg(0, i, size))
	}

	next := 0
	for next < n {
		w := newTestWaiter(0)
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
	if ch.pipeQ[0] != nil {
		t.Fatal("drained pipe queue should be nil")
	}
}

func TestMeshDispatch_OversizedFrameStillShips(t *testing.T) {
	ch := newDispatchTestChannel(1)
	ch.routeFrameLocked(dataMsg(0, 0, 4096))
	w := newTestWaiter(0)
	ch.addWaiterLocked(w)
	ch.flushLocked(1024)
	if b := takeBatch(t, w); len(b) != 1 {
		t.Fatalf("got %d frames, want 1", len(b))
	}
}

// Unkeyed data frames (pings) stay on the data path, spread over the pipes.
func TestMeshDispatch_PingsRoundRobinOnPipes(t *testing.T) {
	ch := newDispatchTestChannel(2)
	for i := 0; i < 4; i++ {
		ch.routeFrameLocked(meshMsg{frameType: frame.FrameLinkPing, payload: []byte("ping"), flowKey: flowKeyAny})
	}
	if len(ch.pipeQ[0]) != 2 || len(ch.pipeQ[1]) != 2 || len(ch.ctrlQ) != 0 || len(ch.pipeAnyQ) != 0 {
		t.Fatalf("ping spread pipe0=%d pipe1=%d ctrl=%d any=%d",
			len(ch.pipeQ[0]), len(ch.pipeQ[1]), len(ch.ctrlQ), len(ch.pipeAnyQ))
	}
}

func TestMeshDispatch_PipeIsolation(t *testing.T) {
	ch := newDispatchTestChannel(2)
	ch.routeFrameLocked(dataMsg(1, 0, 100))
	w0 := newTestWaiter(0)
	ch.addWaiterLocked(w0)
	ch.flushLocked(64 * 1024)
	select {
	case <-w0.batch:
		t.Fatal("pipe 0 waiter received pipe 1 data")
	default:
	}
	w1 := newTestWaiter(1)
	ch.addWaiterLocked(w1)
	ch.flushLocked(64 * 1024)
	takeBatch(t, w1)
}

func TestMeshDispatch_StaleWaiterSkipped(t *testing.T) {
	ch := newDispatchTestChannel(1)
	stale := newTestWaiter(0)
	fresh := newTestWaiter(0)
	ch.addWaiterLocked(stale)
	ch.addWaiterLocked(fresh)
	if b := ch.abandonWaiter(stale); b != nil {
		t.Fatal("abandoned idle waiter returned a batch")
	}
	ch.routeFrameLocked(dataMsg(0, 0, 100))
	ch.flushLocked(64 * 1024)
	takeBatch(t, fresh)
	if len(stale.batch) != 0 {
		t.Fatal("stale waiter received a batch")
	}
}

// A batch handed off just before the handler times out must still be served.
func TestMeshDispatch_AbandonKeepsHandedOffBatch(t *testing.T) {
	ch := newDispatchTestChannel(1)
	w := newTestWaiter(0)
	ch.addWaiterLocked(w)
	ch.routeFrameLocked(dataMsg(0, 7, 100))
	ch.flushLocked(64 * 1024)
	b := ch.abandonWaiter(w)
	if len(b) != 1 || binary.BigEndian.Uint32(b[0].payload) != 7 {
		t.Fatalf("abandon lost handed-off batch: %+v", b)
	}
}

// Before X-N (and always in legacy mode) data rides the wildcard FIFO.
func TestMeshDispatch_WildcardBeforePipeCount(t *testing.T) {
	ch := newDispatchTestChannel(0)
	for i := 0; i < 3; i++ {
		ch.routeFrameLocked(dataMsg(uint32(i), i, 100))
	}
	w := newTestWaiter(0) // pipe unknown yet → wildcard
	ch.addWaiterLocked(w)
	ch.flushLocked(64 * 1024)
	b := takeBatch(t, w)
	for i, m := range b {
		if int(binary.BigEndian.Uint32(m.payload)) != i {
			t.Fatalf("wildcard order broken at %d", i)
		}
	}
	if len(b) != 3 {
		t.Fatalf("got %d frames, want 3", len(b))
	}
}
