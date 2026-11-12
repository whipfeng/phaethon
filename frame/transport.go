package frame

import (
	"fmt"
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
	// Control marks a control frame inside a batch (same meaning as Send's
	// isControl); transports with a control fast path must honor it.
	Control bool
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
	lowat      bool // TCP_NOTSENT_LOWAT in effect on the carrier socket

	// writeMu serializes frame writes on the wire. The p2p layer currently
	// uses one writer goroutine per transport, but FrameTransport does not
	// require that of its callers: two concurrent Send/SendBatch calls would
	// interleave frame headers and payloads on the stream (a partial writev
	// can be preempted), permanently desyncing the peer's frame parser.
	writeMu sync.Mutex
}

// NewStreamTransport wraps conn in a FrameTransport.
func NewStreamTransport(conn net.Conn) FrameTransport {
	t := &streamTransport{
		conn:       conn,
		ctrlRecvCh: make(chan streamRecvResult, 16),
		dataRecvCh: make(chan streamRecvResult, 64),
		closed:     make(chan struct{}),
		lowat:      limitKernelBacklog(conn),
	}
	go t.readLoop()
	return t
}

func (t *streamTransport) KernelBacklogLimited() bool { return t.lowat }

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
	// No write deadline: a slow peer applies TCP backpressure, which must
	// propagate rather than kill the link and every inner flow on it. A
	// dead/wedged peer is detected by the read-side heartbeat timeout,
	// which closes the transport and unblocks any stuck writer.
	if len(payload) > MaxPayload {
		return fmt.Errorf("reverse_frame: payload too large (%d)", len(payload))
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if frameType == FrameMeshPacket {
		traceMeshPacket("frame_write_start", payload)
	}
	var hdr [FrameHeaderSize]byte
	hdr[0] = frameType
	hdr[1] = byte(len(payload) >> 8)
	hdr[2] = byte(len(payload))
	var err error
	if tc, ok := t.conn.(*net.TCPConn); ok && len(payload) > 0 {
		// Merge header+payload into a single writev syscall instead of
		// WriteFrame's two writes (design §3.3).
		bufs := net.Buffers{hdr[:], payload}
		var n int64
		n, err = bufs.WriteTo(tc)
		if err == nil && n != int64(FrameHeaderSize+len(payload)) {
			err = io.ErrShortWrite
		}
	} else {
		err = WriteFrame(t.conn, frameType, payload)
	}
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
// TCP fast path: one writev (net.Buffers) carrying per-frame header+payload
// without copying payloads (design §3.3). Non-TCP connections fall back to
// serializing everything into one buffer and a single write.
func (t *streamTransport) SendBatch(frames []Frame) error {
	if len(frames) == 0 {
		return nil
	}
	if len(frames) == 1 {
		return t.Send(frames[0].Type, frames[0].Payload, false)
	}

	// No write deadline — same rationale as Send.
	t.writeMu.Lock()
	defer t.writeMu.Unlock()

	// Pre-calculate total buffer size and validate payloads (a 2-byte length
	// field cannot hold more than MaxPayload).
	totalSize := 0
	for _, f := range frames {
		if len(f.Payload) > MaxPayload {
			return fmt.Errorf("reverse_frame: payload too large (%d)", len(f.Payload))
		}
		totalSize += FrameHeaderSize + len(f.Payload)
	}

	for _, f := range frames {
		if f.Type == FrameMeshPacket {
			traceMeshPacket("frame_write_start", f.Payload)
		}
	}

	var err error
	var n int64
	if tc, ok := t.conn.(*net.TCPConn); ok {
		// writev fast path: 3-byte header + payload per frame, no payload copy.
		bufs := make(net.Buffers, 0, len(frames)*2)
		hdrs := make([][FrameHeaderSize]byte, len(frames))
		for i, f := range frames {
			hdrs[i][0] = f.Type
			hdrs[i][1] = byte(len(f.Payload) >> 8)
			hdrs[i][2] = byte(len(f.Payload))
			bufs = append(bufs, hdrs[i][:])
			if len(f.Payload) > 0 {
				bufs = append(bufs, f.Payload)
			}
		}
		n, err = bufs.WriteTo(tc)
	} else {
		// Fallback: serialize all frames into one buffer, single write.
		buf := make([]byte, totalSize)
		offset := 0
		for _, f := range frames {
			buf[offset] = f.Type
			buf[offset+1] = byte(len(f.Payload) >> 8)
			buf[offset+2] = byte(len(f.Payload))
			offset += FrameHeaderSize
			if len(f.Payload) > 0 {
				copy(buf[offset:], f.Payload)
				offset += len(f.Payload)
			}
		}
		var nw int
		nw, err = t.conn.Write(buf)
		n = int64(nw)
	}
	if err == nil && n != int64(totalSize) {
		// Partial write without error corrupts the frame stream — the peer
		// would desync on the truncated buffer. Surface it so the caller
		// tears down the transport (same handling as a write error).
		err = io.ErrShortWrite
	}

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
