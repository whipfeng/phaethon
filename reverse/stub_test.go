package reverse

import (
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stubRegistry simulates the registry side (server) of a reverse connection.
type stubRegistry struct {
	ln     net.Listener
	conns  map[net.Conn]struct{}
	mu     sync.Mutex
	closed bool

	sendPings    bool
	delayPeng    time.Duration
	dropAfterN   int
	pingInterval time.Duration

	pingCount   atomic.Int32
	pongCount   atomic.Int32
	handshakeOk atomic.Int32
	connClosed  atomic.Int32
}

func newStubRegistry(addr string) (*stubRegistry, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &stubRegistry{
		ln:           ln,
		conns:        make(map[net.Conn]struct{}),
		sendPings:    true,
		pingInterval: 30 * time.Second,
	}
	go s.acceptLoop()
	return s, nil
}

func (s *stubRegistry) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			conn.Close()
			continue
		}
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		go s.handleConn(conn)
	}
}

func (s *stubRegistry) handleConn(conn net.Conn) {
	defer func() {
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
		conn.Close()
		s.connClosed.Add(1)
	}()

	// Step 1: wait for PENG frame (client confirming registration)
	frameType, _, err := ReadFrame(conn)
	if err != nil {
		return
	}
	if frameType != FramePeng {
		return
	}

	stopPing := make(chan struct{})
	var stopOnce sync.Once
	if s.sendPings {
		go func() {
			ticker := time.NewTicker(s.pingInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					if s.dropAfterN > 0 && int(s.pingCount.Load()) >= s.dropAfterN {
						return
					}
					if err := WriteFrame(conn, FrameHeartbeat, nil); err != nil {
						return
					}
					s.pingCount.Add(1)
				case <-stopPing:
					return
				}
			}
		}()
	}

	// Step 2: wait for PONG frame (Match triggered), reply with PENG
	for {
		conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		frameType, _, err := ReadFrame(conn)
		if err != nil {
			return
		}
		switch frameType {
		case FramePong:
			s.pongCount.Add(1)
			if s.delayPeng > 0 {
				time.Sleep(s.delayPeng)
			}
			if err := WriteFrame(conn, FramePeng, nil); err != nil {
				return
			}
			s.handshakeOk.Add(1)
			stopOnce.Do(func() { close(stopPing) })
			<-make(chan struct{})
			return
		case FrameHeartbeat:
			continue
		default:
			return
		}
	}
}

func (s *stubRegistry) Close() {
	s.mu.Lock()
	s.closed = true
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	s.ln.Close()
	for _, c := range conns {
		c.Close()
	}
}

func (s *stubRegistry) Addr() string {
	return s.ln.Addr().String()
}

func TestReverseLongLivedConnection(t *testing.T) {
	Refresh()
	reg := GlobalRegistry()

	stub, err := newStubRegistry("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer stub.Close()

	addr := stub.Addr()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}

	go HandleReverseConnection(conn, "test-addr")

	time.Sleep(500 * time.Millisecond)

	reg.mu.Lock()
	poolSize := len(reg.bottoms["test-addr"])
	reg.mu.Unlock()
	if poolSize != 1 {
		t.Fatalf("expected pool=1, got %d", poolSize)
	}

	time.Sleep(65 * time.Second)

	reg.mu.Lock()
	poolSize = len(reg.bottoms["test-addr"])
	reg.mu.Unlock()
	if poolSize != 1 {
		t.Fatalf("expected pool=1 after 65s, got %d", poolSize)
	}

	t.Logf("Triggering Match at %v", time.Now())
	matched, err := reg.Match("test-addr")
	t.Logf("Match returned: matched=%v err=%v", matched != nil, err)
	if err != nil {
		t.Fatalf("match failed: %v", err)
	}
	if matched == nil {
		t.Fatal("match returned nil")
	}
	defer matched.Close()

	t.Logf("stub pingCount=%d pongCount=%d handshakeOk=%d", stub.pingCount.Load(), stub.pongCount.Load(), stub.handshakeOk.Load())
	if stub.handshakeOk.Load() != 1 {
		t.Fatalf("expected 1 handshake, got %d", stub.handshakeOk.Load())
	}

	t.Logf("Test passed: connection stayed alive for 65s, pingCount=%d", stub.pingCount.Load())
}

func TestReverseIdleTimeout(t *testing.T) {
	Refresh()
	reg := GlobalRegistry()

	stub, err := newStubRegistry("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stub.sendPings = false
	defer stub.Close()

	addr := stub.Addr()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}

	go HandleReverseConnection(conn, "idle-addr")

	time.Sleep(500 * time.Millisecond)

	reg.mu.Lock()
	poolSize := len(reg.bottoms["idle-addr"])
	reg.mu.Unlock()
	if poolSize != 1 {
		t.Fatalf("expected pool=1, got %d", poolSize)
	}

	time.Sleep(70 * time.Second)

	reg.mu.Lock()
	poolSize = len(reg.bottoms["idle-addr"])
	reg.mu.Unlock()
	if poolSize != 0 {
		t.Fatalf("expected pool=0 after idle timeout, got %d", poolSize)
	}

	if stub.connClosed.Load() != 1 {
		t.Fatalf("expected 1 closed connection, got %d", stub.connClosed.Load())
	}

	t.Log("Test passed: idle connection was correctly closed")
}

func TestReverseDoubleCloseRace(t *testing.T) {
	Refresh()
	reg := GlobalRegistry()

	stub, err := newStubRegistry("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer stub.Close()

	addr := stub.Addr()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}

	go HandleReverseConnection(conn, "race-addr")

	time.Sleep(500 * time.Millisecond)

	matched, err := reg.Match("race-addr")
	if err != nil {
		t.Fatalf("match failed: %v", err)
	}

	matched.Close()

	time.Sleep(500 * time.Millisecond)

	t.Log("Test passed: no double-close race")
}

func TestReverseStaleConnectionCleanup(t *testing.T) {
	Refresh()
	reg := GlobalRegistry()

	stub, err := newStubRegistry("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer stub.Close()

	addr := stub.Addr()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}

	go HandleReverseConnection(conn, "stale-addr")

	time.Sleep(500 * time.Millisecond)

	reg.mu.Lock()
	poolSize := len(reg.bottoms["stale-addr"])
	reg.mu.Unlock()
	if poolSize != 1 {
		t.Fatalf("expected pool=1, got %d", poolSize)
	}

	stub.Close()

	time.Sleep(2 * time.Second)

	reg.mu.Lock()
	poolSize = len(reg.bottoms["stale-addr"])
	reg.mu.Unlock()
	if poolSize != 0 {
		t.Fatalf("expected pool=0 after remote close, got %d", poolSize)
	}

	t.Log("Test passed: stale connection was cleaned up")
}
