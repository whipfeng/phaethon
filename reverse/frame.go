package reverse

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"phaethon/util"
)

// Frame types for the Unified Reverse Frame Protocol.
const (
	FrameHeartbeat  byte = 0x01 // HEARTBEAT: keep-alive ping (empty payload)
	FramePong       byte = 0x02 // PONG: registration accepted (empty payload)
	FramePeng       byte = 0x03 // PENG: registration confirmed (empty payload)
	FrameUDPChannel byte = 0x04 // UDP_CHANNEL: UDP tunnel command (variable payload)
	FrameData       byte = 0x05 // DATA: raw application-layer data
)

// MaxPayload is the maximum frame payload size (16-bit length field).
const MaxPayload = 65535

// ReadFrame reads one complete frame from conn.
// Returns (frameType, payload, error).
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
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
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
			continue // silently consume control frames
		case FrameData:
			if len(payload) > 0 {
				c.writeBuf(payload)
			}
		default:
			util.LogWarn("[REVERSE-FRAMED] unknown frame type 0x%02x from=%s", frameType, c.conn.RemoteAddr())
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
