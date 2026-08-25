package reverse

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"phaethon/util"
)

// ========== Stub Trojan Server ==========

type stubTrojanServer struct {
	ln       net.Listener
	password string

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool

	bindCount    atomic.Int32
	connectCount atomic.Int32
	dataReceived atomic.Int64
	dataSent     atomic.Int64
}

func newStubTrojanServer() (*stubTrojanServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &stubTrojanServer{
		ln:       ln,
		password: util.Sha224Hex("testpass"),
		conns:    make(map[net.Conn]struct{}),
	}
	go s.acceptLoop()
	return s, nil
}

func (s *stubTrojanServer) acceptLoop() {
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

func (s *stubTrojanServer) handleConn(conn net.Conn) {
	bindMode := false
	defer func() {
		if bindMode {
			return
		}
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
		conn.Close()
	}()

	passwordBuf := make([]byte, 56)
	if _, err := io.ReadFull(conn, passwordBuf); err != nil {
		return
	}
	if string(passwordBuf) != s.password {
		return
	}

	crlf := make([]byte, 2)
	if _, err := io.ReadFull(conn, crlf); err != nil {
		return
	}

	cmdAtyp := make([]byte, 2)
	if _, err := io.ReadFull(conn, cmdAtyp); err != nil {
		return
	}
	cmd := cmdAtyp[0]
	atyp := cmdAtyp[1]

	var dstAddr string
	switch atyp {
	case 0x01:
		ipBuf := make([]byte, 4)
		if _, err := io.ReadFull(conn, ipBuf); err != nil {
			return
		}
		dstAddr = net.IP(ipBuf).String()
	case 0x03:
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(conn, lenBuf); err != nil {
			return
		}
		domainBuf := make([]byte, lenBuf[0])
		if _, err := io.ReadFull(conn, domainBuf); err != nil {
			return
		}
		dstAddr = string(domainBuf)
	default:
		return
	}

	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBuf); err != nil {
		return
	}
	_ = binary.BigEndian.Uint16(portBuf)

	if _, err := io.ReadFull(conn, crlf); err != nil {
		return
	}

	if cmd == 0x02 { // BIND -> reverse registration
		bindMode = true
		s.bindCount.Add(1)
		HandleReverseConnection(conn, dstAddr)
		return
	}

	// CONNECT -> echo server
	s.connectCount.Add(1)
	buf := make([]byte, 1024)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		s.dataReceived.Add(int64(n))
		resp := append([]byte("ECHO:"), buf[:n]...)
		if _, err := conn.Write(resp); err != nil {
			return
		}
		s.dataSent.Add(int64(len(resp)))
	}
}

func (s *stubTrojanServer) Close() {
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

func (s *stubTrojanServer) Addr() string {
	return s.ln.Addr().String()
}

// ========== Stub Trojan Client ==========

func stubTrojanClientDialBind(serverAddr, dstAddr string) (net.Conn, error) {
	conn, err := net.Dial("tcp", serverAddr)
	if err != nil {
		return nil, err
	}

	passwordHash := util.Sha224Hex("testpass")
	cmd := byte(0x02)

	var req []byte
	req = append(req, []byte(passwordHash)...)
	req = append(req, 0x0D, 0x0A)
	req = append(req, cmd)
	req = append(req, 0x03)
	req = append(req, byte(len(dstAddr)))
	req = append(req, []byte(dstAddr)...)
	portBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(portBuf, 0)
	req = append(req, portBuf...)
	req = append(req, 0x0D, 0x0A)

	if _, err := conn.Write(req); err != nil {
		conn.Close()
		return nil, err
	}

	// Wait for PENG frame from server
	frameType, _, err := ReadFrame(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("wait for PENG fail: %w", err)
	}
	if frameType != FramePeng {
		conn.Close()
		return nil, fmt.Errorf("expected PENG(0x%02x), got 0x%02x", FramePeng, frameType)
	}

	return conn, nil
}

// runReverseHeartbeat mimics server/reverse.go handleReverseConn using frame protocol.
func runReverseHeartbeat(conn net.Conn) (net.Conn, error) {
	stopPing := make(chan struct{})
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := WriteFrame(conn, FrameHeartbeat, nil); err != nil {
					return
				}
			case <-stopPing:
				return
			}
		}
	}()

	for {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		frameType, _, err := ReadFrame(conn)
		if err != nil {
			return nil, err
		}
		switch frameType {
		case FrameHeartbeat:
			continue
		case FramePong:
			close(stopPing)
			if err := WriteFrame(conn, FramePeng, nil); err != nil {
				return nil, err
			}
			conn.SetReadDeadline(time.Time{})
			return conn, nil
		case FramePeng:
			conn.SetReadDeadline(time.Time{})
			continue
		default:
			return nil, fmt.Errorf("unexpected frame type: 0x%02x", frameType)
		}
	}
}

// ========== Tests ==========

func TestTrojanReverseDataTransfer(t *testing.T) {
	Refresh()
	reg := GlobalRegistry()

	server, err := newStubTrojanServer()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	clientConn, err := stubTrojanClientDialBind(server.Addr(), "rv-addr")
	if err != nil {
		t.Fatalf("client dial bind fail: %v", err)
	}

	matchedCh := make(chan net.Conn, 1)
	errCh := make(chan error, 1)
	go func() {
		relayConn, err := runReverseHeartbeat(clientConn)
		if err != nil {
			errCh <- err
			return
		}
		matchedCh <- relayConn
	}()

	time.Sleep(500 * time.Millisecond)

	reg.mu.Lock()
	poolSize := len(reg.bottoms["rv-addr"])
	reg.mu.Unlock()
	if poolSize != 1 {
		t.Fatalf("expected pool=1, got %d", poolSize)
	}

	matchedConn, err := reg.Match("rv-addr")
	if err != nil {
		t.Fatalf("match failed: %v", err)
	}
	defer matchedConn.Close()

	var relayConn net.Conn
	select {
	case relayConn = <-matchedCh:
	case err := <-errCh:
		t.Fatalf("client handshake failed: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for client-side PONG handling")
	}
	defer relayConn.Close()

	testData := []byte("hello-trojan-reverse-" + strings.Repeat("X", 1000))

	if _, err := matchedConn.Write(testData); err != nil {
		t.Fatalf("dialer write fail: %v", err)
	}

	recvBuf := make([]byte, len(testData))
	if _, err := io.ReadFull(relayConn, recvBuf); err != nil {
		t.Fatalf("client read fail: %v", err)
	}
	if !bytes.Equal(recvBuf, testData) {
		t.Fatalf("data mismatch: dialer->client")
	}

	replyData := []byte("reply-from-client-" + strings.Repeat("Y", 1000))
	if _, err := relayConn.Write(replyData); err != nil {
		t.Fatalf("client write fail: %v", err)
	}

	replyBuf := make([]byte, len(replyData))
	if _, err := io.ReadFull(matchedConn, replyBuf); err != nil {
		t.Fatalf("dialer read fail: %v", err)
	}
	if !bytes.Equal(replyBuf, replyData) {
		t.Fatalf("data mismatch: client->dialer")
	}

	t.Logf("Data transfer OK: %d bytes each direction", len(testData))
}

func TestTrojanReverseHeartbeatNoDataPollution(t *testing.T) {
	Refresh()
	reg := GlobalRegistry()

	server, err := newStubTrojanServer()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	clientConn, err := stubTrojanClientDialBind(server.Addr(), "rv-addr2")
	if err != nil {
		t.Fatalf("client dial bind fail: %v", err)
	}

	matchedCh := make(chan net.Conn, 1)
	errCh := make(chan error, 1)
	go func() {
		relayConn, err := runReverseHeartbeat(clientConn)
		if err != nil {
			errCh <- err
			return
		}
		matchedCh <- relayConn
	}()

	time.Sleep(500 * time.Millisecond)

	time.Sleep(35 * time.Second)

	matchedConn, err := reg.Match("rv-addr2")
	if err != nil {
		t.Fatalf("match failed: %v", err)
	}
	defer matchedConn.Close()

	var relayConn net.Conn
	select {
	case relayConn = <-matchedCh:
	case err := <-errCh:
		t.Fatalf("client handshake failed: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for client-side PONG handling")
	}
	defer relayConn.Close()

	testData := []byte("DATA-" + strings.Repeat("A", 500))
	if _, err := matchedConn.Write(testData); err != nil {
		t.Fatalf("write fail: %v", err)
	}

	recvBuf := make([]byte, len(testData))
	if _, err := io.ReadFull(relayConn, recvBuf); err != nil {
		t.Fatalf("read fail: %v", err)
	}
	if !bytes.Equal(recvBuf, testData) {
		t.Fatalf("data polluted: expected %q, got %q", testData[:20], recvBuf[:20])
	}

	replyData := []byte("REPLY-" + strings.Repeat("B", 500))
	if _, err := relayConn.Write(replyData); err != nil {
		t.Fatalf("write fail: %v", err)
	}

	replyBuf := make([]byte, len(replyData))
	if _, err := io.ReadFull(matchedConn, replyBuf); err != nil {
		t.Fatalf("read fail: %v", err)
	}
	if !bytes.Equal(replyBuf, replyData) {
		t.Fatalf("data polluted on return path")
	}

	t.Log("No heartbeat data pollution detected")
}

func TestTrojanReverseLongLivedWithData(t *testing.T) {
	Refresh()
	reg := GlobalRegistry()

	server, err := newStubTrojanServer()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	clientConn, err := stubTrojanClientDialBind(server.Addr(), "rv-addr3")
	if err != nil {
		t.Fatalf("client dial bind fail: %v", err)
	}

	matchedCh := make(chan net.Conn, 1)
	errCh := make(chan error, 1)
	go func() {
		relayConn, err := runReverseHeartbeat(clientConn)
		if err != nil {
			errCh <- err
			return
		}
		matchedCh <- relayConn
	}()

	time.Sleep(500 * time.Millisecond)

	time.Sleep(65 * time.Second)

	matchedConn, err := reg.Match("rv-addr3")
	if err != nil {
		t.Fatalf("match failed: %v", err)
	}
	defer matchedConn.Close()

	var relayConn net.Conn
	select {
	case relayConn = <-matchedCh:
	case err := <-errCh:
		t.Fatalf("client handshake failed: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for client-side PONG handling")
	}
	defer relayConn.Close()

	for i := 0; i < 5; i++ {
		testData := []byte(fmt.Sprintf("batch-%d-%s", i, strings.Repeat("Z", 200)))
		if _, err := matchedConn.Write(testData); err != nil {
			t.Fatalf("write batch %d fail: %v", i, err)
		}
		recvBuf := make([]byte, len(testData))
		if _, err := io.ReadFull(relayConn, recvBuf); err != nil {
			t.Fatalf("read batch %d fail: %v", i, err)
		}
		if !bytes.Equal(recvBuf, testData) {
			t.Fatalf("batch %d data mismatch", i)
		}
		time.Sleep(100 * time.Millisecond)
	}

	t.Log("Long-lived data exchange OK")
}
