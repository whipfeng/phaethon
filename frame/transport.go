package frame

import (
	"net"
	"time"
)

// FrameTransport is the P2P frame transport abstraction with datagram
// semantics: implementations MAY drop, duplicate or reorder frames
// (all P2P frame consumers tolerate this).
type FrameTransport interface {
	// Send writes one frame. Implementations may buffer asynchronously
	// (nil return does not mean the frame is on the wire); failures are
	// surfaced through a later Send/Recv error.
	Send(frameType byte, payload []byte) error
	// Recv blocks for the next frame. Liveness detection is built into
	// each implementation (stream: 60s read deadline; htunnel direct:
	// long-poll GET cycle).
	Recv() (frameType byte, payload []byte, err error)
	Close() error
}

// streamTransport adapts a reliable, ordered net.Conn to FrameTransport,
// preserving the exact deadline behavior of the former inline P2P write/read
// loops: write 5s (30s for FrameMeshPacket), read 60s.
type streamTransport struct {
	conn net.Conn
}

// NewStreamTransport wraps conn in a FrameTransport.
func NewStreamTransport(conn net.Conn) FrameTransport {
	return &streamTransport{conn: conn}
}

func (t *streamTransport) Send(frameType byte, payload []byte) error {
	deadline := 5 * time.Second
	if frameType == FrameMeshPacket {
		// Bulk mesh data may legitimately stall on slow transports
		// (h_tunnel/trojan); give overlay TCP time to drain instead of
		// tearing the connection down mid-transfer.
		deadline = 30 * time.Second
	}
	_ = t.conn.SetWriteDeadline(time.Now().Add(deadline))
	if err := WriteFrame(t.conn, frameType, payload); err != nil {
		_ = t.conn.SetWriteDeadline(time.Time{})
		return err
	}
	_ = t.conn.SetWriteDeadline(time.Time{})
	return nil
}

func (t *streamTransport) Recv() (byte, []byte, error) {
	_ = t.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	frameType, payload, err := ReadFrame(t.conn)
	_ = t.conn.SetReadDeadline(time.Time{})
	return frameType, payload, err
}

func (t *streamTransport) Close() error {
	return t.conn.Close()
}
