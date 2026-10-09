package frame

import (
	"io"
	"net"
	"sync"
	"time"
)

// FrameTransport is the P2P frame transport abstraction with datagram
// semantics: implementations MAY drop, duplicate or reorder frames
// (all P2P frame consumers tolerate this).
type FrameTransport interface {
	// Send writes one frame. isControl=true marks control frames which
	// implementations MUST NOT block behind bulk data frames.
	Send(frameType byte, payload []byte, isControl bool) error
	// Recv blocks for the next frame. Implementations MUST prioritize
	// control frames over data frames.
	Recv() (frameType byte, payload []byte, err error)
	Close() error
}

type streamRecvResult struct {
	frameType byte
	payload   []byte
	err       error
}

// streamTransport adapts a reliable, ordered net.Conn to FrameTransport.
// A background readLoop reads frames from TCP and routes them to separate
// ctrlRecvCh / dataRecvCh channels, ensuring control frames are never
// delayed behind bulk data frames.
type streamTransport struct {
	conn       net.Conn
	ctrlRecvCh chan streamRecvResult
	dataRecvCh chan streamRecvResult
	closed     chan struct{}
	closeOnce  sync.Once
	writeMu    sync.Mutex
}

// NewStreamTransport wraps conn in a FrameTransport.
func NewStreamTransport(conn net.Conn) FrameTransport {
	t := &streamTransport{
		conn:       conn,
		ctrlRecvCh: make(chan streamRecvResult, 16),
		dataRecvCh: make(chan streamRecvResult, 64),
		closed:     make(chan struct{}),
	}
	go t.readLoop()
	return t
}

func (t *streamTransport) readLoop() {
	for {
		_ = t.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		ft, payload, err := ReadFrame(t.conn)
		_ = t.conn.SetReadDeadline(time.Time{})

		if err != nil {
			select {
			case t.ctrlRecvCh <- streamRecvResult{err: err}:
			case <-t.closed:
			}
			return
		}

		ch := t.dataRecvCh
		if ft != FrameMeshPacket {
			ch = t.ctrlRecvCh
		}
		select {
		case ch <- streamRecvResult{frameType: ft, payload: payload}:
		case <-t.closed:
			return
		}
	}
}

func (t *streamTransport) Send(frameType byte, payload []byte, isControl bool) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()

	deadline := 30 * time.Second
	if isControl {
		deadline = 5 * time.Second
	}
	_ = t.conn.SetWriteDeadline(time.Now().Add(deadline))
	defer t.conn.SetWriteDeadline(time.Time{})
	return WriteFrame(t.conn, frameType, payload)
}

func (t *streamTransport) Recv() (byte, []byte, error) {
	// Priority: non-blocking check for control frames
	select {
	case r := <-t.ctrlRecvCh:
		return r.frameType, r.payload, r.err
	default:
	}

	// Block on either channel
	select {
	case r := <-t.ctrlRecvCh:
		return r.frameType, r.payload, r.err
	case r := <-t.dataRecvCh:
		return r.frameType, r.payload, r.err
	case <-t.closed:
		return 0, nil, io.ErrClosedPipe
	}
}

func (t *streamTransport) Close() error {
	t.closeOnce.Do(func() {
		close(t.closed)
		t.conn.Close()
	})
	return nil
}
