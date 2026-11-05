package p2p

import (
	"sync"

	"phaethon/frame"
)

// firstFrameTransport re-delivers one pre-read frame through Recv, then
// delegates to base. Used when HandleP2PTransport peeks the first frame to
// detect StripeAttach: a primary connection's first frame (v8: gossip) must
// still reach the session readLoop so establishPeerFromGossip sees it.
//
// The injected payload keeps its ReadFrame pool buffer; ownership transfers
// to whoever consumes Recv (the session readLoop returns it).
type firstFrameTransport struct {
	mu    sync.Mutex
	first *frame.Frame
	base  frame.FrameTransport
}

func newFirstFrameTransport(base frame.FrameTransport, ft byte, payload []byte) *firstFrameTransport {
	f := frame.Frame{Type: ft, Payload: payload}
	return &firstFrameTransport{base: base, first: &f}
}

func (t *firstFrameTransport) Send(frameType byte, payload []byte, isControl bool) error {
	return t.base.Send(frameType, payload, isControl)
}

func (t *firstFrameTransport) SendBatch(frames []frame.Frame) error {
	return t.base.SendBatch(frames)
}

func (t *firstFrameTransport) Recv() (byte, []byte, error) {
	t.mu.Lock()
	if t.first != nil {
		f := *t.first
		t.first = nil
		t.mu.Unlock()
		return f.Type, f.Payload, nil
	}
	t.mu.Unlock()
	return t.base.Recv()
}

func (t *firstFrameTransport) Close() error { return t.base.Close() }
