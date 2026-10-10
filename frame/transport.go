package frame

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Frame represents a single frame for batch sending.
type Frame struct {
	Type    byte
	Payload []byte
}

// FrameTransport is the P2P frame transport abstraction with datagram
// semantics: implementations MAY drop, duplicate or reorder frames
// (all P2P frame consumers tolerate this).
type FrameTransport interface {
	// Send writes one frame. isControl=true marks control frames which
	// implementations MUST NOT block behind bulk data frames.
	Send(frameType byte, payload []byte, isControl bool) error
	// SendBatch writes multiple frames as a batch. Implementations should
	// serialize all frames and write them in a single operation where possible.
	// Default behavior: call Send for each frame sequentially.
	SendBatch(frames []Frame) error
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

// MeshPacketTraceHook observes FrameMeshPacket boundaries without affecting transport semantics.
type MeshPacketTraceHook func(stage string, payload []byte)

var meshPacketTraceHook atomic.Value

func SetMeshPacketTraceHook(hook MeshPacketTraceHook) {
	if hook == nil {
		hook = func(string, []byte) {}
	}
	meshPacketTraceHook.Store(hook)
}

func traceMeshPacket(stage string, payload []byte) {
	hook, ok := meshPacketTraceHook.Load().(MeshPacketTraceHook)
	if ok {
		hook(stage, payload)
	}
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

		if ft == FrameMeshPacket {
			traceMeshPacket("frame_read", payload)
		}
		ch := t.dataRecvCh
		if ft != FrameMeshPacket {
			ch = t.ctrlRecvCh
		}
		select {
		case ch <- streamRecvResult{frameType: ft, payload: payload}:
			if ft == FrameMeshPacket {
				traceMeshPacket("frame_inbound_enqueued", payload)
			}
		case <-t.closed:
			return
		}
	}
}

func (t *streamTransport) Send(frameType byte, payload []byte, isControl bool) error {
	deadline := 30 * time.Second
	if isControl {
		deadline = 5 * time.Second
	}
	_ = t.conn.SetWriteDeadline(time.Now().Add(deadline))
	defer t.conn.SetWriteDeadline(time.Time{})
	if frameType == FrameMeshPacket {
		traceMeshPacket("frame_write_start", payload)
	}
	err := WriteFrame(t.conn, frameType, payload)
	if frameType == FrameMeshPacket {
		if err != nil {
			traceMeshPacket("frame_write_error", payload)
		} else {
			traceMeshPacket("frame_write_ok", payload)
		}
	}
	return err
}

// SendBatch writes multiple frames in a single write operation.
// All frames are serialized into a buffer first, then written atomically.
func (t *streamTransport) SendBatch(frames []Frame) error {
	if len(frames) == 0 {
		return nil
	}
	if len(frames) == 1 {
		return t.Send(frames[0].Type, frames[0].Payload, false)
	}

	_ = t.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	defer t.conn.SetWriteDeadline(time.Time{})

	// Serialize all frames into buffer
	var buf []byte
	for _, f := range frames {
		if f.Type == FrameMeshPacket {
			traceMeshPacket("frame_write_start", f.Payload)
		}
		frameBytes := serializeFrame(f.Type, f.Payload)
		buf = append(buf, frameBytes...)
	}

	// Single write for all frames
	_, err := t.conn.Write(buf)

	// Trace results
	for _, f := range frames {
		if f.Type == FrameMeshPacket {
			if err != nil {
				traceMeshPacket("frame_write_error", f.Payload)
			} else {
				traceMeshPacket("frame_write_ok", f.Payload)
			}
		}
	}
	return err
}

// serializeFrame serializes a single frame into bytes (header + payload).
func serializeFrame(frameType byte, payload []byte) []byte {
	var hdr [3]byte
	hdr[0] = frameType
	hdr[1] = byte(len(payload) >> 8)
	hdr[2] = byte(len(payload))
	if len(payload) == 0 {
		return hdr[:]
	}
	result := make([]byte, 3+len(payload))
	copy(result[:3], hdr[:])
	copy(result[3:], payload)
	return result
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
		if r.frameType == FrameMeshPacket && r.err == nil {
			traceMeshPacket("frame_inbound_dequeued", r.payload)
		}
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
