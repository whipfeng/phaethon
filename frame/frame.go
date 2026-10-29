package frame

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"phaethon/util"
)

// Frame types for reverse proxy protocol (used by ReverseFramedConn).
const (
	FrameHeartbeat  byte = 0x01 // Keep-alive ping
	FramePong       byte = 0x02 // Registration accepted
	FramePeng       byte = 0x03 // Registration confirmed
	FrameUDPChannel byte = 0x04 // UDP tunnel command
	FrameData       byte = 0x05 // Raw application data
)

// Frame types for P2P protocol v8.
// Control frames (0x10-0x1F): fire-and-forget, no seq/ack
// Data frames (0x20-0xFF): fire-and-forget, no seq/ack
const (
	// Control frames (fire-and-forget, no seq/ack)
	FrameGossip     byte = 0x12 // Gossip: topology update + keepalive
	FrameLinkPing   byte = 0x14 // Link ping: link quality measurement
	FrameLinkPong   byte = 0x15 // Link pong: link quality response

	// Data frames (fire-and-forget, no seq/ack)
	FrameMeshPacket byte = 0x20 // Mesh overlay IP packet
)

// FrameHeader is the header for all frames (type + len).
// Wire format: {type(1), len(2), payload}
const FrameHeaderSize = 3

// MaxPayload is the maximum frame payload size (16-bit length field).
const MaxPayload = 65535

// readBufferPool pools byte slices for ReadFrame to reduce allocations.
// Callers MUST call ReturnBuffer when done with the payload.
var readBufferPool = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, MaxPayload)
		return buf
	},
}

// GetReadBuffer gets a buffer from the pool (for advanced use cases).
// Most callers should use ReadFrame instead.
func GetReadBuffer() []byte {
	return readBufferPool.Get().([]byte)
}

// ReturnBuffer returns a buffer to the pool.
// MUST be called for every non-nil payload returned by ReadFrame.
// Safe to call with nil or empty slices.
func ReturnBuffer(buf []byte) {
	if len(buf) == 0 {
		return
	}
	if cap(buf) == MaxPayload {
		// Reset length to capacity before returning to pool.
		// This ensures the next Get returns a buffer with len=MaxPayload.
		buf = buf[:MaxPayload]
		readBufferPool.Put(buf)
	}
}

// ReadFrame reads one complete frame from conn.
// Returns (frameType, payload, error).
// IMPORTANT: Caller MUST call ReturnBuffer(payload) when done to avoid memory leaks.
func ReadFrame(r io.Reader) (byte, []byte, error) {
	var hdr [3]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, fmt.Errorf("reverse_frame: read header fail: %w", err)
	}
	frameType := hdr[0]
	length := binary.BigEndian.Uint16(hdr[1:])
	if length > MaxPayload {
		return 0, nil, fmt.Errorf("reverse_frame: payload too large (%d)", length)
	}
	if length == 0 {
		return frameType, nil, nil
	}
	
	// Get buffer from pool
	buf := GetReadBuffer()
	payload := buf[:length]
	
	if _, err := io.ReadFull(r, payload); err != nil {
		ReturnBuffer(payload)  // Return on error
		return 0, nil, fmt.Errorf("reverse_frame: read payload fail: %w", err)
	}
	return frameType, payload, nil
}

// WriteFrame writes one complete frame to w.
func WriteFrame(w io.Writer, frameType byte, payload []byte) error {
	if len(payload) > MaxPayload {
		return fmt.Errorf("reverse_frame: payload too large (%d)", len(payload))
	}
	var hdr [3]byte
	hdr[0] = frameType
	binary.BigEndian.PutUint16(hdr[1:], uint16(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// IsControlFrame returns true if the frame type is a P2P control frame (0x10-0x1F).
func IsControlFrame(frameType byte) bool {
	// P2P v8: control frames are 0x10-0x1F (gossip=0x12, ping=0x14, pong=0x15)
	return frameType >= 0x10 && frameType <= 0x1F
}

// ReverseFramedConn wraps a net.Conn with frame-based multiplexing.
// It filters control frames (HEARTBEAT/PONG/PENG) and exposes only
// DATA payloads through the net.Conn interface — business layers are
// completely unaware of the framing protocol.
//
// A background goroutine sends HEARTBEAT frames periodically.
type ReverseFramedConn struct {
	conn      net.Conn
	readBuf   *bytesBuffer // buffered DATA payload from framing
	readMu    sync.Mutex
	writeMu   sync.Mutex
	closed    chan struct{}
	closeOnce sync.Once
}

// NewReverseFramedConn creates a framed connection wrapping conn.
// The heartbeat goroutine starts immediately.
func NewReverseFramedConn(conn net.Conn) *ReverseFramedConn {
	fc := &ReverseFramedConn{
		conn:    conn,
		readBuf: newBytesBuffer(),
		closed:  make(chan struct{}),
	}
	go fc.heartbeatSender()
	return fc
}

func (c *ReverseFramedConn) Read(b []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()

	for {
		n, err := c.readBuf.Read(b)
		if n > 0 || err != nil {
			return n, err
		}

		frameType, payload, err := ReadFrame(c.conn)
		if err != nil {
			return 0, err
		}

		switch frameType {
		case FrameHeartbeat, FramePong, FramePeng:
			ReturnBuffer(payload) // issue 10: return pooled buffer
			continue              // silently consume control frames
		case FrameData:
			if len(payload) > 0 {
				c.writeBuf(payload)
			}
			ReturnBuffer(payload) // issue 10: return pooled buffer
		default:
			util.LogModuleWarn("frame", "[REVERSE-FRAMED] unknown frame type 0x%02x from=%s", frameType, c.conn.RemoteAddr())
			ReturnBuffer(payload) // issue 10: return pooled buffer
		}
	}
}

// Inject pushes raw bytes into the read buffer so the next Read() call returns them.
// Used when a framing layer has already consumed some bytes that belong to
// the application protocol (e.g. the mode-detection frame's payload).
func (c *ReverseFramedConn) Inject(data []byte) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	c.writeBuf(data)
}

func (c *ReverseFramedConn) Write(b []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := WriteFrame(c.conn, FrameData, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *ReverseFramedConn) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
	})
	return c.conn.Close()
}

// Unwrap returns the underlying raw connection, bypassing the frame layer.
// Used by P2P which runs its own framing (ReadFrame/WriteFrame) on top.
func (c *ReverseFramedConn) Unwrap() net.Conn { return c.conn }

func (c *ReverseFramedConn) LocalAddr() net.Addr                { return c.conn.LocalAddr() }
func (c *ReverseFramedConn) RemoteAddr() net.Addr               { return c.conn.RemoteAddr() }
func (c *ReverseFramedConn) SetDeadline(t time.Time) error      { return c.conn.SetDeadline(t) }
func (c *ReverseFramedConn) SetReadDeadline(t time.Time) error  { return c.conn.SetReadDeadline(t) }
func (c *ReverseFramedConn) SetWriteDeadline(t time.Time) error { return c.conn.SetWriteDeadline(t) }

func (c *ReverseFramedConn) heartbeatSender() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-c.closed:
			return
		case <-ticker.C:
		}
		c.writeMu.Lock()
		if err := WriteFrame(c.conn, FrameHeartbeat, nil); err != nil {
			c.writeMu.Unlock()
			c.Close()
			return
		}
		c.writeMu.Unlock()
	}
}

func (c *ReverseFramedConn) writeBuf(data []byte) {
	c.readBuf.Write(data)
}

// bytesBuffer is a minimal thread-safe bytes buffer for read caching.
type bytesBuffer struct {
	mu  sync.Mutex
	buf []byte
	pos int
}

func newBytesBuffer() *bytesBuffer {
	return &bytesBuffer{buf: make([]byte, 0, 4096)}
}

func (b *bytesBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *bytesBuffer) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pos >= len(b.buf) {
		return 0, nil // empty, caller will read next frame
	}
	n := copy(p, b.buf[b.pos:])
	b.pos += n
	if b.pos >= len(b.buf) {
		b.buf = b.buf[:0]
		b.pos = 0
	}
	return n, nil
}
