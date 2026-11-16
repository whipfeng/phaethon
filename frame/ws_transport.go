package frame

import (
	"bytes"
	"net"
	"sync"

	"golang.org/x/net/websocket"
)

// WSTransport adapts a WebSocket connection to FrameTransport. Each binary
// WebSocket message carries one or more length-prefixed frames, so one write
// can batch many frames and the peer never has to parse HTTP framing.
//
// Seal/Open optionally transform the whole message payload (the h_tunnel
// password crypto); nil means plaintext (e.g. a wss:// endpoint).
type WSTransport struct {
	Conn *websocket.Conn
	Seal func([]byte) []byte
	Open func([]byte) ([]byte, error)

	raw   net.Conn // underlying TCP (or TLS wrapping TCP) for backpressure tuning
	lowat bool     // TCP_NOTSENT_LOWAT is in effect on raw

	mu     sync.Mutex
	ctrlQ  []wsQueued
	dataQ  []wsQueued
	closed bool

	done      chan struct{}
	closeOnce sync.Once

	// stall closes the carrier when a single write is blocked past the
	// configured limit (carrier RTO backoff HOL). Nil when disabled.
	stall *sendStallGuard
}

// NewWSTransport builds a WS carrier and, when the raw connection is known,
// applies the same TCP_NOTSENT_LOWAT backpressure as the plain TCP carrier.
// WS Send is synchronous, so with a bounded kernel backlog the p2p write loop
// can use the same DRR/fair-queue path as TCP instead of the async credit gate.
func NewWSTransport(ws *websocket.Conn, raw net.Conn, seal func([]byte) []byte, open func([]byte) ([]byte, error)) *WSTransport {
	t := &WSTransport{Conn: ws, raw: raw, Seal: seal, Open: open}
	if raw != nil {
		t.lowat = limitKernelBacklog(raw)
	}
	t.stall = newSendStallGuard(carrierSendStallLimit(), func() { t.Close() })
	return t
}

func (t *WSTransport) KernelBacklogLimited() bool { return t.lowat }

// CarrierRetransStats reports the carrier TCP's cumulative segment counters.
func (t *WSTransport) CarrierRetransStats() (uint64, uint64, bool) {
	if t.raw == nil {
		return 0, 0, false
	}
	return CarrierRetransStats(t.raw)
}

func (t *WSTransport) SetKernelBacklogLimit(n int) bool {
	if n <= 0 || t.raw == nil {
		return false
	}
	c := t.raw
	for i := 0; i < 8; i++ {
		if tc, ok := c.(*net.TCPConn); ok {
			if err := setNotSentLowat(tc, n); err != nil {
				return false
			}
			t.lowat = true
			return true
		}
		u, ok := c.(interface{ NetConn() net.Conn })
		if !ok {
			break
		}
		c = u.NetConn()
	}
	return false
}

type wsQueued struct {
	frameType byte
	payload   []byte
}

// StreamCarrier marks a transport that is an ordered byte stream (WebSocket,
// plain TCP, ...). Stripe lanes apply to such carriers exactly as they do to
// TCP; pools of independent HTTP requests (h_tunnel long-poll) are not.
type StreamCarrier interface {
	IsStreamCarrier() bool
}

func (t *WSTransport) IsStreamCarrier() bool { return true }

func (t *WSTransport) Send(frameType byte, payload []byte, isControl bool) error {
	return t.sendFrames([]Frame{{Type: frameType, Payload: payload, Control: isControl}})
}

func (t *WSTransport) SendBatch(frames []Frame) error {
	if len(frames) == 0 {
		return nil
	}
	return t.sendFrames(frames)
}

func (t *WSTransport) sendFrames(frames []Frame) error {
	var buf bytes.Buffer
	for _, f := range frames {
		if err := WriteFrame(&buf, f.Type, f.Payload); err != nil {
			return err
		}
	}
	data := buf.Bytes()
	if t.Seal != nil {
		data = t.Seal(data)
	}
	t.stall.begin()
	defer t.stall.end()
	return websocket.Message.Send(t.Conn, data)
}

func (t *WSTransport) Recv() (byte, []byte, error) {
	t.mu.Lock()
	if len(t.ctrlQ) > 0 {
		q := t.ctrlQ[0]
		t.ctrlQ = t.ctrlQ[1:]
		t.mu.Unlock()
		return q.frameType, q.payload, nil
	}
	if len(t.dataQ) > 0 {
		q := t.dataQ[0]
		t.dataQ = t.dataQ[1:]
		t.mu.Unlock()
		return q.frameType, q.payload, nil
	}
	t.mu.Unlock()

	var msg []byte
	if err := websocket.Message.Receive(t.Conn, &msg); err != nil {
		return 0, nil, err
	}
	if t.Open != nil {
		plain, err := t.Open(msg)
		if err != nil {
			return 0, nil, err
		}
		msg = plain
	}

	r := bytes.NewReader(msg)
	var ctrl, data []wsQueued
	for r.Len() > 0 {
		ft, payload, err := ReadFrame(r)
		if err != nil {
			return 0, nil, err
		}
		if ft == FrameMeshPacket {
			data = append(data, wsQueued{frameType: ft, payload: payload})
		} else {
			ctrl = append(ctrl, wsQueued{frameType: ft, payload: payload})
		}
	}

	t.mu.Lock()
	t.ctrlQ = append(t.ctrlQ, ctrl...)
	t.dataQ = append(t.dataQ, data...)
	if len(t.ctrlQ) > 0 {
		q := t.ctrlQ[0]
		t.ctrlQ = t.ctrlQ[1:]
		t.mu.Unlock()
		return q.frameType, q.payload, nil
	}
	q := t.dataQ[0]
	t.dataQ = t.dataQ[1:]
	t.mu.Unlock()
	return q.frameType, q.payload, nil
}

func (t *WSTransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.stall.stop()
	if t.done == nil {
		t.done = make(chan struct{})
	}
	done := t.done
	t.mu.Unlock()
	t.closeOnce.Do(func() { close(done) })
	return t.Conn.Close()
}

// Done is closed when the transport is closed (or its connection breaks and
// the owner closes it). The WebSocket server handler blocks on it so a
// connection whose ownership was transferred to a stripe lane is not closed
// when the session-start call returns.
func (t *WSTransport) Done() <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done == nil {
		t.done = make(chan struct{})
	}
	return t.done
}
