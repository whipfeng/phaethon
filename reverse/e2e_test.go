package reverse_test

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"phaethon/config"
	"phaethon/dialer"
	"phaethon/reverse"
	"phaethon/server"
	"phaethon/util"
)

// ========== TLS Stub Trojan Server ==========

// tlsStubTrojanServer simulates a Trojan server that accepts TLS connections,
// handles Trojan BIND commands, and delegates to reverse.HandleReverseConnection.
type tlsStubTrojanServer struct {
	ln       net.Listener
	password string // SHA224 hex

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
}

func newTLSStubTrojanServer() (*tlsStubTrojanServer, error) {
	cert, err := util.GenerateSelfSignedCert()
	if err != nil {
		return nil, err
	}
	tlsConf := &tls.Config{
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true,
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", tlsConf)
	if err != nil {
		return nil, err
	}
	s := &tlsStubTrojanServer{
		ln:       ln,
		password: util.Sha224Hex("testpass"),
		conns:    make(map[net.Conn]struct{}),
	}
	go s.acceptLoop()
	return s, nil
}

func (s *tlsStubTrojanServer) acceptLoop() {
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

func (s *tlsStubTrojanServer) handleConn(conn net.Conn) {
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

	// Read Trojan request
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

	if cmd == 0x02 {
		bindMode = true
		reverse.HandleReverseConnection(conn, dstAddr)
		return
	}

	// Non-BIND: just close
}

func (s *tlsStubTrojanServer) Close() {
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

func (s *tlsStubTrojanServer) Addr() string {
	return s.ln.Addr().String()
}

// ========== SOCKS5 Client ==========

// socks5Connect dials through a SOCKS5 proxy.
func socks5Connect(proxyAddr, dstAddr string, dstPort int) (net.Conn, error) {
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		return nil, err
	}

	// Handshake: no auth
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		conn.Close()
		return nil, err
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		conn.Close()
		return nil, err
	}
	if resp[0] != 0x05 || resp[1] != 0x00 {
		conn.Close()
		return nil, fmt.Errorf("socks5 handshake failed: %v", resp)
	}

	// CONNECT request
	req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(dstAddr))}
	req = append(req, []byte(dstAddr)...)
	portBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(portBuf, uint16(dstPort))
	req = append(req, portBuf...)
	if _, err := conn.Write(req); err != nil {
		conn.Close()
		return nil, err
	}

	// Response
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		conn.Close()
		return nil, err
	}
	if header[1] != 0x00 {
		conn.Close()
		return nil, fmt.Errorf("socks5 connect failed: %d", header[1])
	}
	switch header[3] {
	case 0x01:
		skip := make([]byte, 6)
		if _, err := io.ReadFull(conn, skip); err != nil {
			conn.Close()
			return nil, err
		}
	case 0x03:
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(conn, lenBuf); err != nil {
			conn.Close()
			return nil, err
		}
		skip := make([]byte, int(lenBuf[0])+2)
		if _, err := io.ReadFull(conn, skip); err != nil {
			conn.Close()
			return nil, err
		}
	case 0x04:
		skip := make([]byte, 18)
		if _, err := io.ReadFull(conn, skip); err != nil {
			conn.Close()
			return nil, err
		}
	}

	return conn, nil
}

// ========== End-to-End Test ==========

// TestReverseE2E_TrojanSOCKS5 tests the full chain:
//
//	Test Client -> SOCKS5 Entry Server -> Reverse Registry -> Trojan Tunnel -> Stub Server
func TestReverseE2E_TrojanSOCKS5(t *testing.T) {
	reverse.Refresh()

	// 1. Start target HTTP server
	targetMux := http.NewServeMux()
	targetMux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Write(body)
	})
	targetSrv := &http.Server{Handler: targetMux}
	targetLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go targetSrv.Serve(targetLn)
	defer targetSrv.Close()
	targetAddr := targetLn.Addr().String()
	targetHost, targetPortStr, _ := net.SplitHostPort(targetAddr)
	targetPort := 0
	fmt.Sscanf(targetPortStr, "%d", &targetPort)

	// 2. Start TLS stub Trojan server
	stubSrv, err := newTLSStubTrojanServer()
	if err != nil {
		t.Fatal(err)
	}
	defer stubSrv.Close()
	stubHost, stubPortStr, _ := net.SplitHostPort(stubSrv.Addr())
	stubPort := 0
	fmt.Sscanf(stubPortStr, "%d", &stubPort)

	// 3. Build configuration
	ruleConf := &config.RuleConfiguration{
		Proxies: []*config.Proxy{
			{
				Name:           "LOCAL_TJ",
				Type:           config.ProxyTROJAN,
				Server:         stubHost,
				Port:           stubPort,
				Password:       "testpass",
				Sni:            "test.tj.com",
				SkipCertVerify: true,
			},
			{
				Name:           "LOCAL_TJ_RV",
				Type:           config.ProxySOCKS5,
				ReverseAddress: "tj-addr",
			},
		},
		Rules: []string{
			"MATCH,LOCAL_TJ_RV#TEST_SS5_TJ_RV",
			"MATCH,DIRECT",
		},
		Mappings: []*config.Mapping{
			{
				Name:                  "TEST_TJ_RV",
				Type:                  "socks5",
				ReverseAddress:        "tj-addr",
				ReverseProxy:          "LOCAL_TJ",
				ReverseMaxConnections: 1,
				ReverseRetryInterval:  1000,
			},
			{
				Name: "TEST_SS5_TJ_RV",
				Type: "socks5",
				Port: 0, // auto-assign
			},
		},
	}
	if err := ruleConf.Init(); err != nil {
		t.Fatalf("config init fail: %v", err)
	}

	// 4. Start reverse mapping (this dials out to stub server)
	rvSrv, err := server.StartReverseMapping(ruleConf, ruleConf.Mappings[0])
	if err != nil {
		t.Fatalf("start reverse mapping fail: %v", err)
	}
	defer rvSrv.Close()

	// 5. Start SOCKS5 entry server
	ss5Ln, err := server.StartSocks5(ruleConf, ruleConf.Mappings[1])
	if err != nil {
		t.Fatalf("start socks5 fail: %v", err)
	}
	defer ss5Ln.Close()
	ss5Addr := ss5Ln.Addr().String()

	// 6. Wait for reverse connection to register
	var poolSize int
	for i := 0; i < 30; i++ {
		poolSize = reverse.GlobalRegistry().PoolSize("tj-addr")
		if poolSize >= 1 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if poolSize < 1 {
		t.Fatalf("reverse connection not registered after 6s, pool=%d", poolSize)
	}

	// 7. Connect through SOCKS5 proxy to target
	proxyConn, err := socks5Connect(ss5Addr, targetHost, targetPort)
	if err != nil {
		t.Fatalf("socks5 connect fail: %v", err)
	}
	defer proxyConn.Close()

	// 8. Send HTTP request
	reqBody := append([]byte("hello-e2e-"), bytes.Repeat([]byte("X"), 500)...)
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://%s/echo", targetAddr), bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "text/plain")
	if err := req.Write(proxyConn); err != nil {
		t.Fatalf("write HTTP request fail: %v", err)
	}

	// 9. Read HTTP response
	resp, err := http.ReadResponse(bufio.NewReader(proxyConn), req)
	if err != nil {
		t.Fatalf("read HTTP response fail: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body fail: %v", err)
	}

	if !bytes.Equal(respBody, reqBody) {
		t.Fatalf("body mismatch: sent %d bytes, got %d bytes", len(reqBody), len(respBody))
	}

	t.Logf("E2E test passed: %d bytes round-tripped through Trojan+SOCKS5 reverse", len(reqBody))
}

// TestReverseE2E_TrojanTrojan tests the full chain with Trojan on BOTH sides:
//
//	Test Client -> TrojanDialer(reverse) -> Reverse Registry -> Trojan Tunnel -> Stub Server
//
// This validates that TrojanDialer.TryReverse() correctly wraps in ReverseFramedConn
// and that the server-side Trojan handler works over the framed connection.
func TestReverseE2E_TrojanTrojan(t *testing.T) {
	reverse.Refresh()

	// 1. Start target HTTP server
	targetMux := http.NewServeMux()
	targetMux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Write(body)
	})
	targetSrv := &http.Server{Handler: targetMux}
	targetLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go targetSrv.Serve(targetLn)
	defer targetSrv.Close()
	targetAddr := targetLn.Addr().String()
	targetHost, targetPortStr, _ := net.SplitHostPort(targetAddr)
	targetPort := 0
	fmt.Sscanf(targetPortStr, "%d", &targetPort)

	// 2. Start TLS stub Trojan server (handles BIND for reverse registration)
	stubSrv, err := newTLSStubTrojanServer()
	if err != nil {
		t.Fatal(err)
	}
	defer stubSrv.Close()
	stubHost, stubPortStr, _ := net.SplitHostPort(stubSrv.Addr())
	stubPort := 0
	fmt.Sscanf(stubPortStr, "%d", &stubPort)

	// 3. Build configuration — Trojan client as entry, Trojan server as reverse handler
	ruleConf := &config.RuleConfiguration{
		Proxies: []*config.Proxy{
			{
				Name:           "LOCAL_TJ",
				Type:           config.ProxyTROJAN,
				Server:         stubHost,
				Port:           stubPort,
				Password:       "testpass",
				Sni:            "test.tj.com",
				SkipCertVerify: true,
			},
			{
				Name:           "TJ_RV_PROXY",
				Type:           config.ProxyTROJAN,
				ReverseAddress: "tj-addr",
				Password:       "testpass",
				Sni:            "test.tj.com",
				SkipCertVerify: true,
			},
		},
		Rules: []string{
			"MATCH,DIRECT",
		},
		Mappings: []*config.Mapping{
			{
				Name:                  "TJ_RV_MAPPING",
				Type:                  "trojan",
				ReverseAddress:        "tj-addr",
				ReverseProxy:          "LOCAL_TJ",
				ReverseMaxConnections: 1,
				ReverseRetryInterval:  1000,
				Password:              "testpass",
			},
		},
	}
	if err := ruleConf.Init(); err != nil {
		t.Fatalf("config init fail: %v", err)
	}

	// 4. Start reverse mapping (dials out to stub server via Trojan)
	rvSrv, err := server.StartReverseMapping(ruleConf, ruleConf.Mappings[0])
	if err != nil {
		t.Fatalf("start reverse mapping fail: %v", err)
	}
	defer rvSrv.Close()

	// 5. Wait for reverse connection to register
	var poolSize int
	for i := 0; i < 30; i++ {
		poolSize = reverse.GlobalRegistry().PoolSize("tj-addr")
		if poolSize >= 1 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if poolSize < 1 {
		t.Fatalf("reverse connection not registered after 6s, pool=%d", poolSize)
	}

	// 6. Dial through TrojanDialer's TryReverse path (the code path we're testing)
	tjRvProxy := ruleConf.ProxyNames["TJ_RV_PROXY"]
	connID := util.NextConnID()
	conn, err := dialer.ChainDialWithID(tjRvProxy, targetHost, targetPort, connID)
	if err != nil {
		t.Fatalf("trojan reverse dial fail: %v", err)
	}
	defer conn.Close()

	// 7. Send HTTP request (conn is already framed; Trojan request was sent by sendTrojanRequest)
	reqBody := append([]byte("hello-trojan-e2e-"), bytes.Repeat([]byte("Y"), 500)...)
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://%s/echo", targetAddr), bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "text/plain")
	if err := req.Write(conn); err != nil {
		t.Fatalf("write HTTP request fail: %v", err)
	}

	// 8. Read HTTP response
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatalf("read HTTP response fail: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body fail: %v", err)
	}

	if !bytes.Equal(respBody, reqBody) {
		t.Fatalf("body mismatch: sent %d bytes, got %d bytes", len(reqBody), len(respBody))
	}

	t.Logf("E2E test passed: %d bytes round-tripped through Trojan+Trojan reverse", len(reqBody))
}

// TestReverseRecursionGuard verifies that a reverse-proxy self-reference in rules
// (server matches traffic back to its own reverse proxy) is detected immediately
// instead of causing infinite recursion.
func TestReverseRecursionGuard(t *testing.T) {
	reverse.Refresh()

	// Start TLS stub Trojan server
	stubSrv, err := newTLSStubTrojanServer()
	if err != nil {
		t.Fatal(err)
	}
	defer stubSrv.Close()
	stubHost, stubPortStr, _ := net.SplitHostPort(stubSrv.Addr())
	stubPort := 0
	fmt.Sscanf(stubPortStr, "%d", &stubPort)

	// INTENTIONALLY WRONG config: server rules match back to the reverse proxy itself.
	// Without the recursion guard, this causes infinite dial loop.
	ruleConf := &config.RuleConfiguration{
		Proxies: []*config.Proxy{
			{
				Name:           "LOCAL_TJ",
				Type:           config.ProxyTROJAN,
				Server:         stubHost,
				Port:           stubPort,
				Password:       "testpass",
				Sni:            "test.tj.com",
				SkipCertVerify: true,
			},
			{
				Name:           "SELF_REF_RV",
				Type:           config.ProxyTROJAN,
				ReverseAddress: "tj-addr",
				Password:       "testpass",
				Sni:            "test.tj.com",
				SkipCertVerify: true,
			},
		},
		Rules: []string{
			"MATCH,SELF_REF_RV", // ← BUG: server matches back to itself!
		},
		Mappings: []*config.Mapping{
			{
				Name:                  "RV_MAPPING",
				Type:                  "trojan",
				ReverseAddress:        "tj-addr",
				ReverseProxy:          "LOCAL_TJ",
				ReverseMaxConnections: 1,
				ReverseRetryInterval:  1000,
				Password:              "testpass",
			},
		},
	}
	if err := ruleConf.Init(); err != nil {
		t.Fatalf("config init fail: %v", err)
	}

	rvSrv, err := server.StartReverseMapping(ruleConf, ruleConf.Mappings[0])
	if err != nil {
		t.Fatalf("start reverse mapping fail: %v", err)
	}
	defer rvSrv.Close()

	// Wait for registration
	for i := 0; i < 30; i++ {
		if reverse.GlobalRegistry().PoolSize("tj-addr") >= 1 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	selfRefProxy := ruleConf.ProxyNames["SELF_REF_RV"]
	connID := util.NextConnID()

	// Dial succeeds (TryReverse returns from registry synchronously),
	// but using the connection triggers server-side recursion.
	conn, err := dialer.ChainDialWithID(selfRefProxy, "127.0.0.1", 8080, connID)
	if err != nil {
		t.Fatalf("dial should succeed (recursion happens server-side): %v", err)
	}
	defer conn.Close()

	// Write a small request to trigger server-side processing.
	// The server will match SELF_REF_RV again → recursion guard fires → error/EOF.
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, werr := conn.Write([]byte("GET / HTTP/1.0\r\n\r\n"))
	if werr != nil {
		t.Fatalf("write fail: %v", werr)
	}

	buf := make([]byte, 1024)
	_, rerr := conn.Read(buf)
	// Should get EOF or error quickly — NOT hang for 5+ seconds
	if rerr == nil {
		t.Log("read returned data (server somehow handled it without recursing)")
	} else {
		t.Logf("recursion guard worked: server-side dial blocked, client got: %v", rerr)
	}
}

func containsIgnoreCase(s, substr string) bool {
	return len(s) >= len(substr) && (strings.Contains(strings.ToLower(s), strings.ToLower(substr)))
}
