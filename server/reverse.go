package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"phaethon/config"
	"phaethon/dialer"
	"phaethon/reverse"
	"phaethon/util"
)

// ReverseServer manages a pool of outbound reverse connections.
// It continuously dials through a proxy chain using BIND command,
// keeps connections alive via heartbeat, and serves incoming data
// when a PONG is received from the remote side.
type ReverseServer struct {
	BaseServer
	Handler func(conn net.Conn) // called to handle each accepted reverse connection

	proxy    *config.Proxy
	address  string
	maxConns int
	retryMs  int64

	mu          sync.Mutex
	connections map[net.Conn]struct{}
	closed      bool
	closeCh     chan struct{}
	needConnCh  chan struct{}
}

// StartReverse creates and starts a ReverseServer.
func StartReverse(ruleConf *config.RuleConfiguration, mapping *config.Mapping, handler func(net.Conn)) (*ReverseServer, error) {
	proxy, ok := ruleConf.ProxyNames[mapping.ReverseProxy]
	if !ok {
		return nil, fmt.Errorf("reverse: proxy not found: %s", mapping.ReverseProxy)
	}

	maxConns := mapping.ReverseMaxConnections
	if maxConns <= 0 {
		maxConns = 3
	}
	retryMs := mapping.ReverseRetryInterval
	if retryMs <= 0 {
		retryMs = 5000
	}

	srv := &ReverseServer{
		BaseServer:  BaseServer{RuleConf: ruleConf, Mapping: mapping},
		Handler:     handler,
		proxy:       proxy,
		address:     mapping.ReverseAddress,
		maxConns:    maxConns,
		retryMs:     retryMs,
		connections: make(map[net.Conn]struct{}),
		closeCh:     make(chan struct{}),
		needConnCh:  make(chan struct{}, 1),
	}

	go srv.connectionManager()
	srv.needConnCh <- struct{}{}
	return srv, nil
}

func (s *ReverseServer) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.closeCh)
	// Graceful: stop connection manager, let existing connections finish naturally.
	// The registry side will call CloseByAddress when it detects control channel loss,
	// which triggers proper teardown of matched connections.
	s.mu.Unlock()
	util.LogInfo("[REVERSE-SVR] [%s] graceful close: stopping new connections, %d active will drain", s.address, len(s.connections))
}

// CloseForce immediately closes all pooled connections. Use for process shutdown.
func (s *ReverseServer) CloseForce() {
	s.Close()
	s.mu.Lock()
	conns := make([]net.Conn, 0, len(s.connections))
	for conn := range s.connections {
		conns = append(conns, conn)
	}
	s.mu.Unlock()
	for _, conn := range conns {
		conn.Close()
	}
}

func (s *ReverseServer) connectionManager() {
	for {
		select {
		case <-s.closeCh:
			return
		case <-s.needConnCh:
			s.genConnection()
		}
	}
}

func (s *ReverseServer) genConnection() {
	s.mu.Lock()
	currentSize := len(s.connections)
	closed := s.closed
	s.mu.Unlock()

	if closed || currentSize >= s.maxConns {
		return
	}

	util.LogInfo("[REVERSE-SVR] [%s] genConnection: dialing via %s to %s", s.address, s.proxy.Name, s.address)
	conn, err := s.reverseConnect()
	if err != nil {
		util.LogWarn("[REVERSE-SVR] [%s] connect fail, retry after %dms: %v", s.address, s.retryMs, err)
		time.AfterFunc(time.Duration(s.retryMs)*time.Millisecond, s.notifyNeedConn)
		return
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		conn.Close()
		return
	}
	s.connections[conn] = struct{}{}
	s.mu.Unlock()

	util.LogDebug("[REVERSE-SVR] [%s] connected via %s, pool=%d", s.address, s.proxy.Name, currentSize+1)

	go s.handleReverseConn(conn)
	s.notifyNeedConn()
}

func (s *ReverseServer) notifyNeedConn() {
	select {
	case s.needConnCh <- struct{}{}:
	case <-s.closeCh:
	default:
	}
}

func (s *ReverseServer) reverseConnect() (net.Conn, error) {
	conn, err := dialer.ChainDial(s.proxy, s.address, 0)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func (s *ReverseServer) removeConnection(conn net.Conn) {
	s.mu.Lock()
	delete(s.connections, conn)
	s.mu.Unlock()
	s.notifyNeedConn()
}

// handleReverseConn runs the heartbeat loop on a reverse connection.
// Uses Unified Reverse Frame Protocol: all messages are TYPE(1B)+LENGTH(2B)+PAYLOAD.
//
// Flow:
//  1. Send HEARTBEAT frames periodically (during registration phase)
//  2. Read frames: filter HEARTBEAT, react to PONG (match from Registry)
//  3. On PONG: stop own heartbeat, reply PENG, hand off to serveAccepted
func (s *ReverseServer) handleReverseConn(conn net.Conn) {
	shouldClose := true
	removed := false
	defer func() {
		if !removed {
			s.removeConnection(conn)
		}
		if shouldClose {
			conn.Close()
		}
	}()

	stopPing := make(chan struct{})
	var pingStopped atomic.Bool
	var senderWg sync.WaitGroup
	senderWg.Add(1)
	go func() {
		defer senderWg.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if pingStopped.Load() {
					return
				}
				if err := reverse.WriteFrame(conn, reverse.FrameHeartbeat, nil); err != nil {
					return
				}
			case <-stopPing:
				return
			case <-s.closeCh:
				return
			}
		}
	}()

	for {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		frameType, _, err := reverse.ReadFrame(conn)
		if err != nil {
			return
		}

		switch frameType {
		case reverse.FrameHeartbeat:
			continue
		case reverse.FramePong:
			util.LogInfo("[REVERSE] [%s] [conn-N/A] Match Success: Target %s <- Registered node %s", s.address, s.address, conn.RemoteAddr())

			pingStopped.Store(true)
			close(stopPing)
			senderWg.Wait()

			if err := reverse.WriteFrame(conn, reverse.FramePeng, nil); err != nil {
				return
			}

			conn.SetReadDeadline(time.Time{})

			s.removeConnection(conn)
			removed = true
			shouldClose = false

			// Pass raw conn — the handler (StartReverseMapping) wraps it in
			// ReverseFramedConn itself. Wrapping here would cause double-framing.
			s.serveAccepted(conn)
			return
		case reverse.FramePeng:
			continue
		default:
			util.LogError("[REVERSE-SVR] [%s] unexpected frame type: 0x%02x", s.address, frameType)
			return
		}
	}
}

func (s *ReverseServer) serveAccepted(bottomConn net.Conn) {
	connID := util.NextConnID()
	util.LogInfo("[REVERSE-SVR] [%s] [%s] serve accepted %s, remoteAddr=%s, localAddr=%s",
		s.address, connID, s.address, bottomConn.RemoteAddr(), bottomConn.LocalAddr())
	if s.Handler != nil {
		s.Handler(bottomConn)
	}
}

// StartReverseMapping starts a reverse mapping that handles connections according to the mapping type.
// The handler reads a frame to determine mode (TCP or UDP channel),
// then either wraps in ReverseFramedConn for TCP or handles UDP channel setup.
func StartReverseMapping(ruleConf *config.RuleConfiguration, mapping *config.Mapping) (*ReverseServer, error) {

	handler := func(rawConn net.Conn) {
		defer rawConn.Close()

		// Read first frame to determine mode (TCP or UDP channel)
		rawConn.SetReadDeadline(time.Now().Add(10 * time.Second))
		frameType, payload, err := reverse.ReadFrame(rawConn)
		if err != nil {
			return
		}
		rawConn.SetReadDeadline(time.Time{})

		if frameType == reverse.FrameUDPChannel {
			handleReverseUDPChannel(rawConn, ruleConf, mapping, payload)
			return
		}

		// TCP mode: wrap in ReverseFramedConn for heartbeat + data demux
		framedConn := reverse.NewReverseFramedConn(rawConn)

		// Feed the mode-frame's payload back as initial data so the
		// protocol handler (SOCKS5/Trojan/Direct) sees it as the first bytes.
		if len(payload) > 0 {
			framedConn.Inject(payload)
		}

		switch mapping.Type {
		case "socks5":
			srv := &Socks5Server{BaseServer: BaseServer{RuleConf: ruleConf, Mapping: mapping}}
			srv.HandleConn(framedConn)
		case "trojan":
			srv := &TrojanServer{
				BaseServer: BaseServer{RuleConf: ruleConf, Mapping: mapping},
				Password:   util.Sha224Hex(mapping.Password),
			}
			srv.HandleConn(framedConn)
		case "direct":
			srv := &DirectServer{BaseServer: BaseServer{RuleConf: ruleConf, Mapping: mapping}}
			srv.HandleConn(framedConn)
		default:
			handleReverseGeneric(ruleConf, mapping, framedConn)
		}
	}

	return StartReverse(ruleConf, mapping, handler)
}

// handleReverseUDPChannel handles the server-side of a Reverse UDP channel.
//
// Architecture (dual-socket):
//   - Port A (tunnelConn): UDP tunnel through proxy chain for encrypted frame exchange with dialer's Port D
//   - Port B (targetConn): raw UDP socket for direct communication with target services
//   - TCP control channel: framed signaling (FrameUDP_CHANNEL), address exchange, session key negotiation
//
// Data flow:
//
//	Client -> [SOCKS5] -> Entry -> DialPacket -> encrypt -> tunnelConn(A<->D) -> decrypt -> send to target(B)
//	Target reply -> targetConn(B) -> build frame -> encrypt -> tunnelConn(A<->D) -> ... -> Client
func handleReverseUDPChannel(tcpConn net.Conn, ruleConf *config.RuleConfiguration, mapping *config.Mapping, payload []byte) {
	connID := util.NextConnID()
	util.LogInfo("[REVERSE-UDP] [%s] [%s] UDP channel request from %s", mapping.Name, connID, tcpConn.RemoteAddr())

	// Dialer's Port D address is the payload of the UDP_CHANNEL frame
	// (sent by client's DialPacket before waiting for our response).
	dialerChainAddr := string(payload)
	if dialerChainAddr == "" {
		util.LogError("[REVERSE-UDP] [%s] [%s] empty dialer addr in frame payload", mapping.Name, connID)
		return
	}
	util.LogInfo("[REVERSE-UDP] [%s] [%s] dialer addr: %s", mapping.Name, connID, dialerChainAddr)

	dialerAddr, err := net.ResolveUDPAddr("udp", dialerChainAddr)
	if err != nil {
		util.LogError("[REVERSE-UDP] [%s] resolve dialer addr fail: %v", mapping.Name, err)
		return
	}

	if dialerAddr.IP == nil || dialerAddr.IP.IsUnspecified() {
		tcpRemote := tcpConn.RemoteAddr().(*net.TCPAddr)
		ip := tcpRemote.IP
		if ip == nil || ip.IsUnspecified() {
			ip = net.ParseIP("127.0.0.1")
		}
		dialerAddr = &net.UDPAddr{IP: ip, Port: dialerAddr.Port}
	}

	sessionKey := generateSessionKey()
	keyHex := hex.EncodeToString(sessionKey[:])
	util.LogInfo("[REVERSE-UDP] [%s] [%s] sending session key", mapping.Name, connID)
	if _, err := fmt.Fprintf(tcpConn, "%s\n", keyHex); err != nil {
		util.LogError("[REVERSE-UDP] [%s] send session key fail: %v", mapping.Name, err)
		return
	}
	crypto := util.NewReverseCrypto(sessionKey)

	reverseProxy, ok := ruleConf.ProxyNames[mapping.ReverseProxy]
	if !ok || reverseProxy == nil {
		util.LogError("[REVERSE-UDP] [%s] reverse proxy not found: %s", mapping.Name, mapping.ReverseProxy)
		return
	}
	var tunnelConn net.PacketConn
	if reverseProxy.Next != nil && reverseProxy.Next.Type != config.ProxyDIRECT {
		tunnelConn, err = dialer.ChainUDPDial(reverseProxy.Next)
	} else {
		tunnelConn, err = dialer.ListenUDP()
	}
	if err != nil {
		util.LogError("[REVERSE-UDP] [%s] create tunnel udp fail: %v", mapping.Name, err)
		return
	}

	targetConn, err := dialer.ListenUDP()
	if err != nil {
		util.LogError("[REVERSE-UDP] [%s] listen target udp fail: %v", mapping.Name, err)
		tunnelConn.Close()
		return
	}

	tunnelLocalAddr := tunnelConn.LocalAddr().String()
	util.LogInfo("[REVERSE-UDP] [%s] [%s] sending tunnel addr=%s", mapping.Name, connID, tunnelLocalAddr)
	if _, err := fmt.Fprintf(tcpConn, "%s\n", tunnelLocalAddr); err != nil {
		util.LogError("[REVERSE-UDP] [%s] send tunnel addr fail: %v", mapping.Name, err)
		tunnelConn.Close()
		targetConn.Close()
		return
	}

	util.LogInfo("[REVERSE-UDP] [%s] [%s] sending READY", mapping.Name, connID)
	if _, err := fmt.Fprintf(tcpConn, "READY\n"); err != nil {
		util.LogError("[REVERSE-UDP] [%s] send READY fail: %v", mapping.Name, err)
		tunnelConn.Close()
		targetConn.Close()
		return
	}

	closed := make(chan struct{})
	var closeOnce sync.Once
	doClose := func() {
		closeOnce.Do(func() {
			close(closed)
			tunnelConn.Close()
			targetConn.Close()
			tcpConn.Close()
		})
	}

	// Mutable dialer address — updated when we receive valid packets from the dialer.
	// This handles NAT remap scenarios where the dialer's UDP source address changes.
	var currentDialerAddr net.Addr = dialerAddr
	var dialerAddrMu sync.Mutex

	// TCP control keepalive: send-only heartbeat to prevent idle timeout.
	// A separate reader goroutine drains incoming frames so the TCP receive
	// buffer never fills up and cause zero-window blocking.
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-closed:
				return
			case <-ticker.C:
			}
			if err := reverse.WriteFrame(tcpConn, reverse.FrameHeartbeat, nil); err != nil {
				doClose()
				return
			}
		}
	}()

	// TCP control reader: drains incoming frames to prevent receive buffer overflow.
	go func() {
		for {
			select {
			case <-closed:
				return
			default:
			}
			tcpConn.SetReadDeadline(time.Now().Add(70 * time.Second))
			_, _, err := reverse.ReadFrame(tcpConn)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue
				}
				doClose()
				return
			}
		}
	}()

	// UDP heartbeat: send periodic encrypted heartbeat frames to dialer's Port D.
	go func() {
		hbFrame := util.BuildReverseUDPHeartbeat()
		ciphertext := crypto.Seal(hbFrame)
		dialerAddrMu.Lock()
		dstAddr := currentDialerAddr
		dialerAddrMu.Unlock()
		util.LogInfo("[REVERSE-UDP] [%s] [%s] sending initial UDP heartbeat to %s", mapping.Name, connID, dstAddr)
		if _, err := tunnelConn.WriteTo(ciphertext, dstAddr); err != nil {
			util.LogInfo("[REVERSE-UDP] [%s] [%s] UDP heartbeat initial send fail: %v", mapping.Name, connID, err)
			doClose()
			return
		}
		util.LogInfo("[REVERSE-UDP] [%s] [%s] initial UDP heartbeat sent ok", mapping.Name, connID)
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-closed:
				return
			case <-ticker.C:
			}
			// Double-check closed before sealing/sending — prevents stray
			// heartbeat from an old session reaching a new listener on the
			// same UDP port (port reuse) and causing auth-fail noise.
			select {
			case <-closed:
				return
			default:
			}
			ciphertext = crypto.Seal(hbFrame)
			dialerAddrMu.Lock()
			dstAddr := currentDialerAddr
			dialerAddrMu.Unlock()
			if _, err := tunnelConn.WriteTo(ciphertext, dstAddr); err != nil {
				util.LogInfo("[REVERSE-UDP] [%s] [%s] UDP heartbeat send fail: %v", mapping.Name, connID, err)
				doClose()
				return
			}
		}
	}()

	connID = util.NextConnID()
	util.LogInfo("[REVERSE-UDP] [%s] [%s] channel established, tunnel=%s target=%s dialer=%s key=%x...",
		mapping.Name, connID, tunnelConn.LocalAddr(), targetConn.LocalAddr(), dialerAddr, sessionKey[:4])

	seenTargets := util.NewFIFOSet(maxSeenTargets)
	var seenMu sync.Mutex
	logSeen := func(key string, isInbound bool) {
		seenMu.Lock()
		first := seenTargets.Put(key)
		seenMu.Unlock()
		if first {
			dir := "->"
			if isInbound {
				dir = "<-"
			}
			util.LogInfo("[REVERSE-UDP-SVR] [%s] [%s] UDP %s %s", mapping.Name, connID, dir, key)
		}
	}

	// Goroutine 1: Tunnel -> Target (decrypt + forward)
	go func(gid string) {
		buf := make([]byte, 65535)
		for {
			select {
			case <-closed:
				return
			default:
			}

			tunnelConn.SetReadDeadline(time.Now().Add(60 * time.Second))
			n, fromAddr, err := tunnelConn.ReadFrom(buf)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue
				}
				if util.IsClosedErr(err) {
					util.LogDebug("[REVERSE-UDP] [%s] [%s] G1 tunnel closed (normal): %v", mapping.Name, gid, err)
				} else {
					util.LogInfo("[REVERSE-UDP] [%s] [%s] G1 tunnel read err: %v", mapping.Name, gid, err)
				}
				doClose()
				return
			}

			plaintext, err := crypto.Open(buf[:n])
			if err != nil {
				util.LogWarn("[REVERSE-UDP] [%s] [%s] G1 decrypt fail from=%s n=%d err=%v", mapping.Name, gid, fromAddr, n, err)
				continue
			}

			// Update dialer address from verified packet source (handles NAT remap).
			dialerAddrMu.Lock()
			currentDialerAddr = fromAddr
			dialerAddrMu.Unlock()

			targetAddr, payload, err := util.ParseReverseUDPFrame(plaintext)
			if err != nil {
				util.LogWarn("[REVERSE-UDP] [%s] [%s] G1 parse fail from=%s n=%d err=%v", mapping.Name, gid, fromAddr, n, err)
				continue
			}

			// Heartbeat frame: ATYP=0xFF returns (nil,nil,nil)
			if targetAddr == nil {
				util.LogDebug("[REVERSE-UDP] [%s] [%s] G1 heartbeat from=%s", mapping.Name, gid, fromAddr)
				continue
			}

			logSeen(targetAddr.String(), false)
			util.LogDebug("[REVERSE-UDP] [%s] [%s] G1 recv tunnel=%s target=%s n=%d", mapping.Name, gid, fromAddr, targetAddr, len(payload))

			if nw, err := targetConn.WriteTo(payload, targetAddr); err != nil {
				util.LogError("[REVERSE-UDP] [%s] [%s] G1 write target FAIL %s: %v", mapping.Name, gid, targetAddr, err)
			} else {
				util.LogDebug("[REVERSE-UDP] [%s] [%s] G1 write target OK %s: wrote=%d", mapping.Name, gid, targetAddr, nw)
			}
		}
	}(connID)

	// Goroutine 2: Target -> Tunnel (frame + encrypt + reply)
	go func(gid string) {
		buf := make([]byte, 65535)
		for {
			select {
			case <-closed:
				return
			default:
			}

			targetConn.SetReadDeadline(time.Now().Add(60 * time.Second))
			n, srcAddr, err := targetConn.ReadFrom(buf)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue
				}
				if util.IsClosedErr(err) {
					util.LogDebug("[REVERSE-UDP] [%s] [%s] G2 target closed (normal): %v", mapping.Name, gid, err)
				} else {
					util.LogInfo("[REVERSE-UDP] [%s] [%s] G2 target read err: %v", mapping.Name, gid, err)
				}
				doClose()
				return
			}
			logSeen(srcAddr.String(), true)
			util.LogDebug("[REVERSE-UDP] [%s] [%s] G2 recv target=%s n=%d", mapping.Name, gid, srcAddr, n)

			frame := util.BuildReverseUDPFrame(srcAddr, buf[:n])
			if frame == nil {
				util.LogWarn("[REVERSE-UDP] [%s] [%s] G2 build frame fail target=%s n=%d", mapping.Name, gid, srcAddr, n)
				continue
			}

			ciphertext := crypto.Seal(frame)
			dialerAddrMu.Lock()
			dstAddr := currentDialerAddr
			dialerAddrMu.Unlock()
			if dstAddr == nil {
				util.LogWarn("[REVERSE-UDP] [%s] [%s] G2 dialerAddr is nil, dropping reply from %s", mapping.Name, gid, srcAddr)
				continue
			}
			util.LogDebug("[REVERSE-UDP] [%s] [%s] G2 write tunnel dst=%s n=%d", mapping.Name, gid, dstAddr, len(ciphertext))
			if _, err := tunnelConn.WriteTo(ciphertext, dstAddr); err != nil {
				util.LogError("[REVERSE-UDP] [%s] [%s] G2 tunnel write err: %v", mapping.Name, gid, err)
				doClose()
				return
			}
		}
	}(connID)

	<-closed
	util.LogInfo("[REVERSE-UDP] [%s] [%s] channel closed", mapping.Name, connID)
}

func generateSessionKey() [32]byte {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		panic("reverse_udp: failed to generate session key: " + err.Error())
	}
	return key
}

func handleReverseGeneric(ruleConf *config.RuleConfiguration, mapping *config.Mapping, clientConn net.Conn) {
	dstHost := mapping.DstHost
	dstPort := mapping.DstPort
	if dstHost == "" || dstPort == 0 {
		return
	}

	req := config.NewConnectRequest(dstHost, dstPort)
	req = ruleConf.Resolving(req)

	proxy := ruleConf.Match(req, mapping)
	if proxy == nil || strings.ToUpper(proxy.Type) == config.ProxyREJECT {
		return
	}

	connID := util.NextConnID()
	targetConn, err := dialer.ChainDialWithID(proxy, req.DstAddr, req.DstPort, connID)
	if err != nil {
		util.LogError("[REVERSE-GENERIC] [%s] [%s] connect fail %s:%d: %v", mapping.Name, connID, req.DstAddr, req.DstPort, err)
		return
	}
	defer targetConn.Close()

	util.LogInfo("[REVERSE-GENERIC] [%s] [%s] %s -> %s:%d via %s(%s)", mapping.Name, connID, clientConn.RemoteAddr(), req.DstAddr, req.DstPort, proxy.Name, proxy.Type)
	util.RelayWithRateLimit(clientConn, targetConn, proxy.UpRateLimiter, proxy.DownRateLimiter)
}
