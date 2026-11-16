package frame

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// blockingConn is a net.Conn whose Write blocks until Close. It models a
// carrier socket whose kernel send buffer cannot drain because the peer's
// ACK stream is stuck in RTO backoff.
type blockingConn struct {
	closed   chan struct{}
	closeOne sync.Once
}

func newBlockingConn() *blockingConn { return &blockingConn{closed: make(chan struct{})} }

func (c *blockingConn) Read([]byte) (int, error) {
	<-c.closed
	return 0, io.EOF
}

func (c *blockingConn) Write([]byte) (int, error) {
	<-c.closed
	return 0, io.ErrClosedPipe
}

func (c *blockingConn) Close() error {
	c.closeOne.Do(func() { close(c.closed) })
	return nil
}

func (c *blockingConn) LocalAddr() net.Addr              { return fakeAddr("local") }
func (c *blockingConn) RemoteAddr() net.Addr             { return fakeAddr("remote") }
func (c *blockingConn) SetDeadline(time.Time) error      { return nil }
func (c *blockingConn) SetReadDeadline(time.Time) error  { return nil }
func (c *blockingConn) SetWriteDeadline(time.Time) error { return nil }

type fakeAddr string

func (a fakeAddr) Network() string { return "fake" }
func (a fakeAddr) String() string  { return string(a) }

// TestSendStallGuardResetsBlockedCarrier proves a write that stays blocked
// past PHAETHON_CARRIER_STALL_MS closes the carrier so the caller (lane writer
// / session) can re-establish it instead of waiting out TCP RTO backoff.
func TestSendStallGuardResetsBlockedCarrier(t *testing.T) {
	t.Setenv("PHAETHON_CARRIER_STALL_MS", "100")
	conn := newBlockingConn()
	tr := NewStreamTransport(conn)

	done := make(chan struct{})
	go func() {
		_ = tr.Send(FrameMeshPacket, []byte("payload"), false)
		close(done)
	}()

	select {
	case <-conn.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("carrier was not reset after the write stall limit")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Send did not return after the carrier was closed")
	}
}

// TestSendStallGuardDisabledKeepsLegacyBehavior pins the default: without the
// env switch a slow write is allowed to keep blocking (existing backpressure
// contract), and the guard never closes the carrier.
func TestSendStallGuardDisabledKeepsLegacyBehavior(t *testing.T) {
	t.Setenv("PHAETHON_CARRIER_STALL_MS", "")
	conn := newBlockingConn()
	tr := NewStreamTransport(conn)

	done := make(chan struct{})
	go func() {
		_ = tr.Send(FrameMeshPacket, []byte("payload"), false)
		close(done)
	}()

	select {
	case <-conn.closed:
		t.Fatal("carrier closed while the stall guard was disabled")
	case <-time.After(250 * time.Millisecond):
	}
	_ = tr.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Send did not return after explicit Close")
	}
}
