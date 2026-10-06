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

// Frame types for P2P protocol v7.
// Control frames (0x10-0x1F): reliable delivery with seq/ack
// Data frames (0x20-0xFF): fire-and-forget, no seq/ack
const (
	// Control frames (reliable, with seq/ack)
	FrameHello  byte = 0x11 // Hello: connection handshake
	FrameGossip byte = 0x12 // Gossip: topology update + keepalive
	FrameAck    byte = 0x13 // Pure ACK: immediate acknowledgment (no payload)

	// Data frames (fire-and-forget, no seq/ack)
	FrameMeshPacket byte = 0x20 // Mesh overlay IP packet
)

// ControlFrameHeader is the header for control frames (type + seq + ack).
// Wire format: {type(1), seq(4), ack(4), len(2), payload}
const ControlFrameHeaderSize = 11

// DataFrameHeader is the header for data frames (type + len).
// Wire format: {type(1), len(2), payload}
const DataFrameHeaderSize = 3

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

// ControlFrame represents a control frame with seq/ack for reliable delivery.
type ControlFrame struct {
	Type    byte
	Seq     uint32
	Ack     uint32
	Payload []byte
}

// ReadControlFrame reads a control frame with seq/ack from r.
// Wire format: {type(1), seq(4), ack(4), len(2), payload}
func ReadControlFrame(r io.Reader) (*ControlFrame, error) {
	var hdr [ControlFrameHeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, fmt.Errorf("control_frame: read header fail: %w", err)
	}

	f := &ControlFrame{
		Type: hdr[0],
		Seq:  binary.BigEndian.Uint32(hdr[1:5]),
		Ack:  binary.BigEndian.Uint32(hdr[5:9]),
	}

	length := binary.BigEndian.Uint16(hdr[9:11])
	if length > MaxPayload {
		return nil, fmt.Errorf("control_frame: payload too large (%d)", length)
	}
	if length > 0 {
		f.Payload = make([]byte, length)
		if _, err := io.ReadFull(r, f.Payload); err != nil {
			return nil, fmt.Errorf("control_frame: read payload fail: %w", err)
		}
	}
	return f, nil
}

// WriteControlFrame writes a control frame with seq/ack to w.
// Wire format: {type(1), seq(4), ack(4), len(2), payload}
func WriteControlFrame(w io.Writer, frameType byte, seq, ack uint32, payload []byte) error {
	if len(payload) > MaxPayload {
		return fmt.Errorf("control_frame: payload too large (%d)", len(payload))
	}
	var hdr [ControlFrameHeaderSize]byte
	hdr[0] = frameType
	binary.BigEndian.PutUint32(hdr[1:5], seq)
	binary.BigEndian.PutUint32(hdr[5:9], ack)
	binary.BigEndian.PutUint16(hdr[9:11], uint16(len(payload)))
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

// IsControlFrame returns true if the frame type is a control frame (0x01-0x0F).
func IsControlFrame(frameType byte) bool {
	// P2P v7: control frames are 0x10-0x1F (hello=0x11, gossip=0x12)
	// Reverse proxy control frames are 0x01-0x04 (heartbeat, pong, peng, udp_channel)
	return (frameType >= 0x01 && frameType <= 0x0F) || (frameType >= 0x10 && frameType <= 0x1F)
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
