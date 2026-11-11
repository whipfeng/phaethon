package frame

import (
	"bytes"
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

	mu     sync.Mutex
	ctrlQ  []wsQueued
	dataQ  []wsQueued
	closed bool
}

type wsQueued struct {
	frameType byte
	payload   []byte
}

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
	t.mu.Unlock()
	return t.Conn.Close()
}
