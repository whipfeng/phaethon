package dialer_test

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"phaethon/config"
	"phaethon/reverse"
	"phaethon/server"
)

// ===========================================================================
//  E2E Tests: Reverse UDP full-topology verification
//
//  These tests verify the complete reverse-UDP data path through all layers:
//    [Client] → SOCKS5 Entry → Reverse Registry → Trojan tunnel
//    → Stub Server → handleReverseConnection → Rev-side SOCKS5
//    → handleReverseUDPChannel → Target Echo
//
//  Two topologies are tested:
//    1. Minimal (no middle proxy) — simplest path, easiest to debug
//    2. Mid-proxy — middle SOCKS5 between rev-side and echo target
//
//  Important: Each UDP packet through the reverse tunnel consumes a pool
//  connection. Multi-packet tests use ReverseMaxConnections=1 with pool
//  refill waits between packets (refillInterval=200ms).
// ===========================================================================

// ---- stub TLS Trojan server ----
type e2eStubTrojanServer struct {
	ln       net.Listener
	password string
	mu       sync.Mutex
	conns    map[net.Conn]struct{}
	closed   bool
}

func newE2EStubTrojan(t *testing.T) *e2eStubTrojanServer {
	t.Helper()
	cert := generateE2ESelfSignedCert(t)
	tlsConf := &tls.Config{Certificates: []tls.Certificate{cert}}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", tlsConf)
	if err != nil {
		t.Fatal(err)
	}
	s := &e2eStubTrojanServer{
		ln:       ln,
		password: sha224Hex("testpass"),
		conns:    make(map[net.Conn]struct{}),
	}
	go s.acceptLoop()
	return s
}

func (s *e2eStubTrojanServer) acceptLoop() {
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

func (s *e2eStubTrojanServer) handleConn(conn net.Conn) {
	bindMode := false
	defer func() {
		if !bindMode {
			s.mu.Lock()
			delete(s.conns, conn)
			s.mu.Unlock()
			conn.Close()
		}
	}()

	buf := make([]byte, 56)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return
	}
	if string(buf) != s.password {
		return
	}
	if _, err := io.ReadFull(conn, make([]byte, 2)); err != nil {
		return // CRLF
	}
	cmdAtyp := make([]byte, 2)
	if _, err := io.ReadFull(conn, cmdAtyp); err != nil {
		return
	}

	var skipLen int
	switch cmdAtyp[1] {
	case 0x01:
		skipLen = 8 // 4(ip) + 2(port) + 2(crlf)
	case 0x03:
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(conn, lenBuf); err != nil {
			return
		}
		skipLen = int(lenBuf[0]) + 4 // domain + port(2) + crlf(2)
	default:
		return
	}
	if _, err := io.ReadFull(conn, make([]byte, skipLen)); err != nil {
		return
	}

	if cmdAtyp[0] == 0x02 { // BIND
		bindMode = true
		reverse.HandleReverseConnection(conn, "tj-addr")
	}
}

func (s *e2eStubTrojanServer) Close() {
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

func (s *e2eStubTrojanServer) Addr() string { return s.ln.Addr().String() }

// ---- UDP echo server ----
type e2eEchoServer struct {
	conn net.PacketConn
}

func startE2EEchoServer(t *testing.T) *e2eEchoServer {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &e2eEchoServer{conn: conn}
	go s.echoLoop()
	return s
}

func (s *e2eEchoServer) echoLoop() {
	buf := make([]byte, 65535)
	for {
		n, src, err := s.conn.ReadFrom(buf)
		if err != nil {
			return
		}
		s.conn.WriteTo(buf[:n], src)
	}
}

func (s *e2eEchoServer) Close() error { return s.conn.Close() }
func (s *e2eEchoServer) Addr() string { return s.conn.LocalAddr().String() }

// ---- SOCKS5 UDP helper ----

func socks5UDPAssociateRaw(proxyAddr string) (net.Conn, *net.UDPAddr, error) {
	conn, err := net.DialTimeout("tcp", proxyAddr, 5*time.Second)
	if err != nil {
		return nil, nil, err
	}
	tcpConn := conn.(*net.TCPConn)

	// No-auth
	if _, err := tcpConn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		tcpConn.Close()
		return nil, nil, err
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(tcpConn, resp); err != nil {
		tcpConn.Close()
		return nil, nil, err
	}
	if resp[0] != 0x05 || resp[1] != 0x00 {
		tcpConn.Close()
		return nil, nil, fmt.Errorf("auth rejected: %x", resp)
	}

	// UDP ASSOCIATE
	req := []byte{0x05, 0x03, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
	if _, err := tcpConn.Write(req); err != nil {
		tcpConn.Close()
		return nil, nil, err
	}
	header := make([]byte, 4)
	if _, err := io.ReadFull(tcpConn, header); err != nil {
		tcpConn.Close()
		return nil, nil, err
	}
	if header[1] != 0x00 {
		tcpConn.Close()
		return nil, nil, fmt.Errorf("udp associate fail: %d", header[1])
	}

	if header[3] != 0x01 {
		tcpConn.Close()
		return nil, nil, fmt.Errorf("unexpected atyp: %d", header[3])
	}
	addrBuf := make([]byte, 6)
	if _, err := io.ReadFull(tcpConn, addrBuf); err != nil {
		tcpConn.Close()
		return nil, nil, err
	}
	port := int(binary.BigEndian.Uint16(addrBuf[4:6]))

	return tcpConn, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port}, nil
}

func buildSocks5UDPPacket(dstHost string, dstPort int, payload []byte) []byte {
	hdr := []byte{0x00, 0x00, 0x00} // RSV + FRAG
	ip := net.ParseIP(dstHost)
	if ip4 := ip.To4(); ip4 != nil {
		hdr = append(hdr, 0x01)
		hdr = append(hdr, ip4...)
	} else {
		hdr = append(hdr, 0x03)
		hdr = append(hdr, byte(len(dstHost)))
		hdr = append(hdr, []byte(dstHost)...)
	}
	portBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(portBuf, uint16(dstPort))
	hdr = append(hdr, portBuf...)
	return append(hdr, payload...)
}

func parseSocks5UDPPayload(pkt []byte) ([]byte, error) {
	if len(pkt) < 10 {
		return nil, fmt.Errorf("too short: %d", len(pkt))
	}
	var hdrLen int
	switch pkt[3] {
	case 0x01:
		hdrLen = 10
	case 0x03:
		hdrLen = 7 + int(pkt[4])
	case 0x04:
		hdrLen = 22
	default:
		return nil, fmt.Errorf("unknown atyp: %d", pkt[3])
	}
	if hdrLen > len(pkt) {
		return nil, fmt.Errorf("header overflow")
	}
	return pkt[hdrLen:], nil
}

// sendSinglePacket sends one SOCKS5 UDP packet via relayAddr using clientUDP,
// waits for the echo response, and returns the response payload.
// clientUDP may be nil (in which case a temporary socket is created).
func sendSinglePacket(t *testing.T, clientUDP net.PacketConn, relayAddr *net.UDPAddr, echoAddr string, payload []byte) []byte {
	t.Helper()

	ownSocket := clientUDP == nil
	if ownSocket {
		var err error
		clientUDP, err = net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("client udp listen: %v", err)
		}
		defer clientUDP.Close()
	}

	_, echoPort, _ := net.SplitHostPort(echoAddr)
	port, _ := strconv.Atoi(echoPort)
	pkt := buildSocks5UDPPacket("127.0.0.1", port, payload)

	clientUDP.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := clientUDP.WriteTo(pkt, relayAddr); err != nil {
		t.Fatalf("send fail: %v", err)
	}

	clientUDP.SetReadDeadline(time.Now().Add(15 * time.Second))
	buf := make([]byte, 65535)
	n, _, err := clientUDP.ReadFrom(buf)
	if err != nil {
		t.Fatalf("recv fail: %v", err)
	}

	resp, err := parseSocks5UDPPayload(buf[:n])
	if err != nil {
		t.Fatalf("parse fail: %v", err)
	}
	return resp
}

// waitForReversePool polls until the reverse pool has at least minSize
// entries, or times out.
func waitForReversePool(t *testing.T, addr string, minSize int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if reverse.GlobalRegistry().PoolSize(addr) >= minSize {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s pool >= %d", addr, minSize)
}

// buildMinimalConfig returns a RuleConfiguration for the simple topology:
//
//	Client → SOCKS5 Entry → Reverse → Trojan TLS → Stub → DIRECT → Echo
//
// maxPool controls ReverseMaxConnections; refillIntervalMs controls retry delay.
func buildMinimalConfig(stubPort, maxPool int, refillIntervalMs int64) *config.RuleConfiguration {
	return &config.RuleConfiguration{
		Proxies: []*config.Proxy{
			{
				Name:           "TROJAN_HOP",
				Type:           config.ProxyTROJAN,
				Server:         "127.0.0.1",
				Port:           stubPort,
				Password:       "testpass",
				Sni:            "test.local",
				SkipCertVerify: true,
			},
			{
				Name:           "REVERSE_VIRTUAL",
				Type:           config.ProxySOCKS5,
				ReverseAddress: "tj-addr",
			},
		},
		Rules: []string{
			"MATCH,REVERSE_VIRTUAL#SS5_ENTRY",
			"MATCH,DIRECT",
		},
		Mappings: []*config.Mapping{
			{
				Name:                  "REV_MAPPING",
				Type:                  "socks5",
				ReverseAddress:        "tj-addr",
				ReverseProxy:          "TROJAN_HOP",
				ReverseMaxConnections: maxPool,
				ReverseRetryInterval:  refillIntervalMs,
			},
			{
				Name: "SS5_ENTRY",
				Type: "socks5",
				Port: 0,
			},
		},
	}
}

// startMinimalChain starts the minimal reverse chain (stub, reverse, entry)
// and returns the entry listener, a cleanup function, and stub/entry addresses.
func startMinimalChain(t *testing.T) (*e2eEchoServer, *e2eStubTrojanServer, *server.ReverseServer, *config.RuleConfiguration, net.Listener) {
	t.Helper()

	echoSrv := startE2EEchoServer(t)
	stub := newE2EStubTrojan(t)

	_, stubPortStr, _ := net.SplitHostPort(stub.Addr())
	stubPort, _ := strconv.Atoi(stubPortStr)

	ruleConf := buildMinimalConfig(stubPort, 1, 200)
	if err := ruleConf.Init(); err != nil {
		t.Fatalf("config init: %v", err)
	}

	rvSrv, err := server.StartReverseMapping(ruleConf, ruleConf.Mappings[0])
	if err != nil {
		t.Fatalf("reverse start: %v", err)
	}

	entryLn, err := server.StartSocks5(ruleConf, ruleConf.Mappings[1])
	if err != nil {
		t.Fatalf("entry start: %v", err)
	}

	return echoSrv, stub, rvSrv, ruleConf, entryLn
}

// ===========================================================================
//  Test 1: Minimal E2E — single packet round-trip
// ===========================================================================

func TestReverseUDPE2E_Minimal(t *testing.T) {
	reverse.Refresh()
	defer reverse.Refresh()

	echoSrv, stub, rvSrv, _, entryLn := startMinimalChain(t)
	defer echoSrv.Close()
	defer stub.Close()
	defer rvSrv.Close()
	defer entryLn.Close()

	waitForReversePool(t, "tj-addr", 1, 10*time.Second)

	tcpCtrl, relayAddr, err := socks5UDPAssociateRaw(entryLn.Addr().String())
	if err != nil {
		t.Fatalf("udp associate: %v", err)
	}
	defer tcpCtrl.Close()

	payload := []byte("hello-minimal-e2e")
	resp := sendSinglePacket(t, nil, relayAddr, echoSrv.Addr(), payload)

	if !bytes.Equal(resp, payload) {
		t.Fatalf("payload mismatch:\n  sent: %q\n  got:  %q", payload, resp)
	}

	t.Logf("Minimal E2E OK: %d bytes round-tripped", len(payload))
}

// ===========================================================================
//  Test 2: Multiple packets on the same client socket
//
//  Each UDP packet consumes one pool connection. This verifies the pool
//  refills and subsequent packets succeed after waiting for the refill.
// ===========================================================================

func TestReverseUDPE2E_MultiplePackets(t *testing.T) {
	reverse.Refresh()
	defer reverse.Refresh()

	echoSrv, stub, rvSrv, _, entryLn := startMinimalChain(t)
	defer echoSrv.Close()
	defer stub.Close()
	defer rvSrv.Close()
	defer entryLn.Close()

	waitForReversePool(t, "tj-addr", 1, 10*time.Second)

	tcpCtrl, relayAddr, err := socks5UDPAssociateRaw(entryLn.Addr().String())
	if err != nil {
		t.Fatalf("udp associate: %v", err)
	}
	defer tcpCtrl.Close()

	// Reuse same client socket for all packets
	clientUDP, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("client udp listen: %v", err)
	}
	defer clientUDP.Close()

	for i := 0; i < 3; i++ {
		waitForReversePool(t, "tj-addr", 1, 5*time.Second)

		payload := []byte(fmt.Sprintf("multi-packet-%d-%s", i, strings.Repeat("x", i*50)))
		resp := sendSinglePacket(t, clientUDP, relayAddr, echoSrv.Addr(), payload)

		if !bytes.Equal(resp, payload) {
			t.Fatalf("packet %d mismatch:\n  sent: %q\n  got:  %q", i, payload, resp)
		}
		t.Logf("Packet %d OK: %d bytes", i, len(payload))
	}

	t.Log("Multiple packets E2E OK")
}

// ===========================================================================
//  Test 3: Data integrity — varying payload sizes
// ===========================================================================

func TestReverseUDPE2E_PayloadSizes(t *testing.T) {
	reverse.Refresh()
	defer reverse.Refresh()

	echoSrv, stub, rvSrv, _, entryLn := startMinimalChain(t)
	defer echoSrv.Close()
	defer stub.Close()
	defer rvSrv.Close()
	defer entryLn.Close()

	waitForReversePool(t, "tj-addr", 1, 10*time.Second)

	tcpCtrl, relayAddr, err := socks5UDPAssociateRaw(entryLn.Addr().String())
	if err != nil {
		t.Fatalf("udp associate: %v", err)
	}
	defer tcpCtrl.Close()

	clientUDP, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("client udp listen: %v", err)
	}
	defer clientUDP.Close()

	sizes := []int{0, 1, 64, 512, 1400}
	for _, size := range sizes {
		waitForReversePool(t, "tj-addr", 1, 5*time.Second)

		payload := make([]byte, size)
		for i := range payload {
			payload[i] = byte((i + size) % 256)
		}

		resp := sendSinglePacket(t, clientUDP, relayAddr, echoSrv.Addr(), payload)

		if !bytes.Equal(resp, payload) {
			t.Fatalf("size=%d mismatch at byte %d", size, firstMismatch(resp, payload))
		}
		t.Logf("Size %d OK", size)
	}

	t.Log("Payload sizes E2E OK")
}

func firstMismatch(a, b []byte) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	if len(a) != len(b) {
		if len(a) < len(b) {
			return len(a)
		}
		return len(b)
	}
	return -1
}

// ===========================================================================
//  Test 4: Mid-proxy E2E (SOCKS5 relay between rev-side and echo)
//
//  Topology:
//    Client → Entry SOCKS5 → Reverse → Trojan → Stub
//    → handleReverse → Rev-SOCKS5 → Middle SOCKS5 → DIRECT → Echo
//
//  Note: The middle SOCKS5 participates in the TCP control path
//  (rule matching on reverse side). The UDP data path in
//  handleReverseUDPChannel currently resolves to DIRECT because
//  TROJAN_HOP has no .Next proxy configured.
// ===========================================================================

func TestReverseUDPE2E_MidProxy(t *testing.T) {
	reverse.Refresh()
	defer reverse.Refresh()

	// Target echo
	echoSrv := startE2EEchoServer(t)
	defer echoSrv.Close()

	// Middle SOCKS5 proxy
	middleRuleConf := &config.RuleConfiguration{
		Proxies: []*config.Proxy{
			{Name: "MID_DIRECT", Type: config.ProxyDIRECT},
		},
		Rules:    []string{"MATCH,DIRECT"},
		Mappings: []*config.Mapping{{Name: "MID_ENTRY", Type: "socks5", Port: 0}},
	}
	if err := middleRuleConf.Init(); err != nil {
		t.Fatalf("middle config: %v", err)
	}
	middleLn, err := server.StartSocks5(middleRuleConf, middleRuleConf.Mappings[0])
	if err != nil {
		t.Fatalf("middle start: %v", err)
	}
	defer middleLn.Close()
	_, midPortStr, _ := net.SplitHostPort(middleLn.Addr().String())
	midPort, _ := strconv.Atoi(midPortStr)

	// Stub Trojan
	stub := newE2EStubTrojan(t)
	defer stub.Close()
	_, stubPortStr, _ := net.SplitHostPort(stub.Addr())
	stubPort, _ := strconv.Atoi(stubPortStr)

	// Main config with middle proxy in reverse-side rule chain
	ruleConf := &config.RuleConfiguration{
		Proxies: []*config.Proxy{
			{
				Name:           "TROJAN_HOP",
				Type:           config.ProxyTROJAN,
				Server:         "127.0.0.1",
				Port:           stubPort,
				Password:       "testpass",
				Sni:            "test.local",
				SkipCertVerify: true,
			},
			{
				Name:           "REVERSE_VIRTUAL",
				Type:           config.ProxySOCKS5,
				ReverseAddress: "tj-addr",
			},
			{
				Name:   "MIDDLE_PROXY",
				Type:   config.ProxySOCKS5,
				Server: "127.0.0.1",
				Port:   midPort,
			},
		},
		Rules: []string{
			"MATCH,REVERSE_VIRTUAL#SS5_ENTRY",
			"MATCH,MIDDLE_PROXY#REV_SIDE",
			"MATCH,DIRECT",
		},
		Mappings: []*config.Mapping{
			{
				Name:                  "REV_SIDE",
				Type:                  "socks5",
				ReverseAddress:        "tj-addr",
				ReverseProxy:          "TROJAN_HOP",
				ReverseMaxConnections: 1,
				ReverseRetryInterval:  500,
			},
			{
				Name: "SS5_ENTRY",
				Type: "socks5",
				Port: 0,
			},
		},
	}
	if err := ruleConf.Init(); err != nil {
		t.Fatalf("config init: %v", err)
	}

	rvSrv, err := server.StartReverseMapping(ruleConf, ruleConf.Mappings[0])
	if err != nil {
		t.Fatalf("reverse start: %v", err)
	}
	defer rvSrv.Close()

	entryLn, err := server.StartSocks5(ruleConf, ruleConf.Mappings[1])
	if err != nil {
		t.Fatalf("entry start: %v", err)
	}
	defer entryLn.Close()

	waitForReversePool(t, "tj-addr", 1, 10*time.Second)

	tcpCtrl, relayAddr, err := socks5UDPAssociateRaw(entryLn.Addr().String())
	if err != nil {
		t.Fatalf("udp associate: %v", err)
	}
	defer tcpCtrl.Close()

	payload := []byte("hello-mid-proxy-e2e")
	resp := sendSinglePacket(t, nil, relayAddr, echoSrv.Addr(), payload)

	if !bytes.Equal(resp, payload) {
		t.Fatalf("payload mismatch:\n  sent: %q\n  got:  %q", payload, resp)
	}

	t.Logf("Mid-proxy E2E OK: %d bytes round-tripped", len(payload))
}

// ===========================================================================
//  Crypto helpers
// ===========================================================================

func generateE2ESelfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func sha224Hex(s string) string {
	h := sha256.Sum224([]byte(s))
	return fmt.Sprintf("%x", h)
}
