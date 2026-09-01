package server

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"phaethon/config"
	"phaethon/connlog"
	"phaethon/dialer"
	"phaethon/reverse"
	"phaethon/util"
)

// Socks5Server handles SOCKS5 protocol
type Socks5Server struct {
	BaseServer
}

func (s *Socks5Server) Serve(listener net.Listener) {
	AcceptLoop(listener, s, "socks5")
}

func (s *Socks5Server) HandleConn(clientConn net.Conn) {
	// Enforce a read deadline during SOCKS5 handshake so a malicious or
	// stalled client cannot hold a goroutine forever.
	if ds, ok := clientConn.(interface{ SetReadDeadline(time.Time) error }); ok {
		ds.SetReadDeadline(time.Now().Add(30 * time.Second))
	}

	shouldClose := true
	defer func() {
		if shouldClose {
			clientConn.Close()
		}
	}()

	needAuth := s.Mapping.Password != ""

	// Read initial request: VER | NMETHODS | METHODS
	header := make([]byte, 2)
	if _, err := io.ReadFull(clientConn, header); err != nil {
		return
	}
	if header[0] != 0x05 {
		return
	}
	methods := make([]byte, header[1])
	if _, err := io.ReadFull(clientConn, methods); err != nil {
		return
	}

	if needAuth {
		// Check if PASSWORD method is offered
		hasPassword := false
		for _, m := range methods {
			if m == 0x02 {
				hasPassword = true
				break
			}
		}
		if !hasPassword {
			clientConn.Write([]byte{0x05, 0xFF}) // NO ACCEPTABLE METHODS
			return
		}
		clientConn.Write([]byte{0x05, 0x02}) // PASSWORD

		// Read auth: VER | ULEN | UNAME | PLEN | PASSWD
		authVer := make([]byte, 1)
		if _, err := io.ReadFull(clientConn, authVer); err != nil {
			return
		}
		ulenBuf := make([]byte, 1)
		if _, err := io.ReadFull(clientConn, ulenBuf); err != nil {
			return
		}
		uname := make([]byte, ulenBuf[0])
		if _, err := io.ReadFull(clientConn, uname); err != nil {
			return
		}
		plenBuf := make([]byte, 1)
		if _, err := io.ReadFull(clientConn, plenBuf); err != nil {
			return
		}
		passwd := make([]byte, plenBuf[0])
		if _, err := io.ReadFull(clientConn, passwd); err != nil {
			return
		}

		if string(uname) != s.Mapping.Username || string(passwd) != s.Mapping.Password {
			clientConn.Write([]byte{0x01, 0x01}) // Auth failure
			return
		}
		clientConn.Write([]byte{0x01, 0x00}) // Auth success
	} else {
		clientConn.Write([]byte{0x05, 0x00}) // NO AUTH
	}

	// Read command request: VER | CMD | RSV | ATYP | DST.ADDR | DST.PORT
	cmdHeader := make([]byte, 4)
	if _, err := io.ReadFull(clientConn, cmdHeader); err != nil {
		return
	}
	cmd := cmdHeader[1]
	atyp := cmdHeader[3]

	var dstAddr string
	switch atyp {
	case 0x01: // IPv4
		ipBuf := make([]byte, 4)
		if _, err := io.ReadFull(clientConn, ipBuf); err != nil {
			return
		}
		dstAddr = net.IP(ipBuf).String()
	case 0x03: // Domain
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(clientConn, lenBuf); err != nil {
			return
		}
		domainBuf := make([]byte, lenBuf[0])
		if _, err := io.ReadFull(clientConn, domainBuf); err != nil {
			return
		}
		dstAddr = string(domainBuf)
	case 0x04: // IPv6
		ipBuf := make([]byte, 16)
		if _, err := io.ReadFull(clientConn, ipBuf); err != nil {
			return
		}
		dstAddr = net.IP(ipBuf).String()
	default:
		sendSocks5Response(clientConn, 0x08) // Address type not supported
		return
	}

	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(clientConn, portBuf); err != nil {
		return
	}
	dstPort := int(binary.BigEndian.Uint16(portBuf))

	if cmd == 0x02 { // BIND -> reverse registration
		util.LogInfo("[SOCKS5-SVR] [%s] BIND received for %s:%d from %s", s.Mapping.Name, dstAddr, dstPort, clientConn.RemoteAddr())
		if dstPort == reverse.BindPortControl {
			// Control connection: PORT=1 means this is a control channel
			sendSocks5Response(clientConn, 0x00) // success
			handleControlConnection(clientConn, dstAddr)
			shouldClose = false
			return
		}
		if dstPort != reverse.BindPortData {
			util.LogInfo("[SOCKS5-SVR] [%s] BIND rejected: invalid port %d (only 0 or 1 allowed)", s.Mapping.Name, dstPort)
			sendSocks5Response(clientConn, 0x05) // connection refused
			return
		}
		// Data connection: PORT=0 only, goes to Registry
		if !s.RuleConf.HasReverseAddress(dstAddr) {
			// Fallback: check if this is a dynamically allocated address
			if GlobalControlManager == nil || !GlobalControlManager.IsDynamicAddress(dstAddr) {
				sendSocks5Response(clientConn, 0x05) // connection refused
				return
			}
		}
		sendSocks5Response(clientConn, 0x00) // success
		reverse.HandleReverseConnection(clientConn, dstAddr)
		shouldClose = false
		return
	}

	if cmd == 0x03 { // UDP ASSOCIATE
		s.handleUDPAssociate(clientConn, &shouldClose)
		return
	}

	if cmd != 0x01 { // Only CONNECT supported
		sendSocks5Response(clientConn, 0x07) // Command not supported
		return
	}

	// Resolve and match
	req := config.NewConnectRequest(dstAddr, dstPort)
	req = s.RuleConf.Resolving(req)

	proxy := s.RuleConf.Match(req, s.Mapping)
	if proxy == nil {
		util.LogInfo("[SOCKS5-SVR] [%s] [conn-N/A] all proxies dead, rejecting %s:%d", s.Mapping.Name, req.DstAddr, req.DstPort)
		connlog.Log("SOCKS5:"+s.Mapping.Name, "TCP", clientConn.RemoteAddr().String(), req.DstAddr, req.DstPort, "", "fail", fmt.Errorf("all proxies dead"))
		sendSocks5Response(clientConn, 0x04) // Host unreachable
		return
	}
	if strings.ToUpper(proxy.Type) == config.ProxyREJECT {
		util.LogInfo("[SOCKS5-SVR] [%s] [conn-N/A] rejected %s:%d", s.Mapping.Name, req.DstAddr, req.DstPort)
		connlog.Log("SOCKS5:"+s.Mapping.Name, "TCP", clientConn.RemoteAddr().String(), req.DstAddr, req.DstPort, "", "reject", nil)
		sendSocks5Response(clientConn, 0x04) // Host unreachable
		return
	}

	connID := util.NextConnID()
	// Log as soon as the client request is known, before the outbound proxy
	// connection is established. A separate "connect fail" or "-> ... via" line
	// will follow once the dial result is known.
	util.LogInfo("[SOCKS5-SVR] [%s] [%s] %s -> %s:%d via %s(%s) connecting", s.Mapping.Name, connID, clientConn.RemoteAddr(), req.DstAddr, req.DstPort, proxy.Name, proxy.Type)

	targetConn, err := dialer.ChainDialWithID(proxy, req.DstAddr, req.DstPort, connID)
	if err != nil {
		util.LogInfo("[SOCKS5-SVR] [%s] [%s] connect fail %s:%d: %v", s.Mapping.Name, connID, req.DstAddr, req.DstPort, err)
		connlog.Log("SOCKS5:"+s.Mapping.Name, "TCP", clientConn.RemoteAddr().String(), req.DstAddr, req.DstPort, proxy.Name, "fail", err)
		sendSocks5Response(clientConn, 0x05) // Connection refused
		return
	}
	defer targetConn.Close()

	// Send success response
	sendSocks5Response(clientConn, 0x00)
	connlog.Log("SOCKS5:"+s.Mapping.Name, "TCP", clientConn.RemoteAddr().String(), req.DstAddr, req.DstPort, proxy.Name, "ok", nil)

	// Handshake complete — clear the deadline so the relay idle timeout
	// (enforced inside RelayWithRateLimit) takes over.
	if ds, ok := clientConn.(interface{ SetReadDeadline(time.Time) error }); ok {
		ds.SetReadDeadline(time.Time{})
	}

	util.LogInfo("[SOCKS5-SVR] [%s] [%s] %s -> %s:%d via %s(%s)", s.Mapping.Name, connID, clientConn.RemoteAddr(), req.DstAddr, req.DstPort, proxy.Name, proxy.Type)
	util.RelayWithRateLimit(clientConn, targetConn, proxy.UpRateLimiter, proxy.DownRateLimiter)
}

func sendSocks5Response(conn net.Conn, status byte) {
	// VER | REP | RSV | ATYP | BND.ADDR | BND.PORT
	resp := []byte{0x05, status, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
	conn.Write(resp)
}

func (s *Socks5Server) handleUDPAssociate(clientConn net.Conn, shouldClose *bool) {
	// clientLn: receives UDP packets from the client (independent port)
	clientLn, err := dialer.ListenUDP()
	if err != nil {
		sendSocks5Response(clientConn, 0x01) // general failure
		return
	}

	udpAddr := clientLn.LocalAddr().(*net.UDPAddr)

	// Send success response: BND.ADDR=0.0.0.0, BND.PORT=allocated port
	resp := []byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0}
	portBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(portBuf, uint16(udpAddr.Port))
	resp = append(resp, portBuf...)
	if _, err := clientConn.Write(resp); err != nil {
		clientLn.Close()
		return
	}

	// Handshake complete — clear the deadline so the TCP control connection
	// stays alive for the entire UDP relay lifetime.
	if ds, ok := clientConn.(interface{ SetReadDeadline(time.Time) error }); ok {
		ds.SetReadDeadline(time.Time{})
	}

	// Keepalive period left at default 30s (configured by base.go SetTCPKeepAlive).
	// Previously overridden to 10m to work around idle UDP relay timeout, but
	// the real fix should be in the protocol/application layer, not TCP keepalive.

	*shouldClose = false // TCP control connection stays open

	relay := &socks5UDPRelay{
		clientConn:  clientConn,
		clientLn:    clientLn,
		ruleConf:    s.RuleConf,
		mapping:     s.Mapping,
		proxyConns:  make(map[string]*udpProxyConn),
		seenTargets: util.NewFIFOSet(maxSeenTargets),
		closed:      make(chan struct{}),
	}

	// Monitor TCP control connection; close UDP relay on disconnect.
	// Loop reading so that client keepalive bytes are ignored and the
	// relay stays alive until the connection is actually closed.
	// UDP relay lifecycle follows the control connection per RFC 1928.
	go func() {
		buf := make([]byte, 1)
		for {
			n, err := clientConn.Read(buf)
			if n == 0 && err == io.EOF {
				util.LogInfo("[SOCKS5-SVR] [%s] [%s] monitor: EOF, closing relay", s.Mapping.Name, clientConn.RemoteAddr())
				relay.Close()
				return
			}
			if err != nil {
				util.LogInfo("[SOCKS5-SVR] [%s] [%s] monitor: read error=%v, closing relay", s.Mapping.Name, clientConn.RemoteAddr(), err)
				relay.Close()
				return
			}
			// n > 0: client sent keepalive data, ignore per RFC 1928
			util.LogDebug("[SOCKS5-SVR] [%s] [%s] monitor: received %d bytes on control conn (ignored)", s.Mapping.Name, clientConn.RemoteAddr(), n)
		}
	}()

	util.LogInfo("[SOCKS5-SVR] [%s] [%s] UDP ASSOCIATE started on port %d", s.Mapping.Name, clientConn.RemoteAddr(), udpAddr.Port)

	relay.run()
}

// udpProxyConn holds a downstream PacketConn and its read goroutine.
type udpProxyConn struct {
	pc     net.PacketConn
	cancel context.CancelFunc
	dead   int32 // atomic: set to 1 when proxyReadLoop exits
}

const maxSeenTargets = 64

// socks5UDPRelay handles UDP packet forwarding for a single SOCKS5 UDP ASSOCIATE session.
// It routes client packets through proxy chains based on RuleConf.Match.
type socks5UDPRelay struct {
	clientConn   net.Conn
	clientLn     net.PacketConn // receives client SOCKS5 UDP requests
	ruleConf     *config.RuleConfiguration
	mapping      *config.Mapping
	proxyConns   map[string]*udpProxyConn // proxy name -> downstream PacketConn
	seenTargets  *util.FIFOSet            // targets already logged (FIFO, capped at maxSeenTargets)
	proxyMu      sync.Mutex
	closed       chan struct{}
	closeOnce    sync.Once
	addrMu       sync.Mutex   // protects clientAddr
	clientAddr   net.Addr     // learned from first valid client packet
	earlyReplies []earlyReply // buffered replies before clientAddr is set
	earlyMu      sync.Mutex   // protects earlyReplies
}

type earlyReply struct {
	data    []byte
	srcAddr net.Addr
}

const maxEarlyReplies = 128

func (r *socks5UDPRelay) run() {
	defer r.Close()
	defer func() {
		r.proxyMu.Lock()
		for _, upc := range r.proxyConns {
			upc.cancel()
			upc.pc.Close()
		}
		r.proxyConns = nil
		r.proxyMu.Unlock()
	}()

	buf := make([]byte, 65535)

	for {
		select {
		case <-r.closed:
			return
		default:
		}

		r.clientLn.SetReadDeadline(time.Now().Add(60 * time.Second))
		n, srcAddr, err := r.clientLn.ReadFrom(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			if util.IsClosedErr(err) {
				util.LogDebug("[SOCKS5-SVR] [%s] UDP client read closed (normal): %v", r.mapping.Name, err)
			} else {
				util.LogWarn("[SOCKS5-SVR] [%s] UDP client read error: %v", r.mapping.Name, err)
			}
			return
		}

		if n < 10 {
			util.LogWarn("[SOCKS5-SVR] [%s] UDP client packet too short: n=%d from=%s", r.mapping.Name, n, srcAddr)
			continue
		}
		// Parse SOCKS5 UDP request header
		if binary.BigEndian.Uint16(buf[0:2]) != 0 || buf[2] != 0 {
			util.LogWarn("[SOCKS5-SVR] [%s] UDP client packet bad header: rsv=%x frag=%x from=%s", r.mapping.Name, binary.BigEndian.Uint16(buf[0:2]), buf[2], srcAddr)
			continue
		}

		atyp := buf[3]
		offset := 4
		var dstAddr string
		var dstPort int

		switch atyp {
		case 0x01: // IPv4
			if n < offset+6 {
				continue
			}
			dstAddr = net.IP(buf[offset : offset+4]).String()
			dstPort = int(binary.BigEndian.Uint16(buf[offset+4 : offset+6]))
			offset += 6
		case 0x03: // Domain
			if n < offset+1 {
				continue
			}
			domainLen := int(buf[offset])
			offset++
			if n < offset+domainLen+2 {
				continue
			}
			dstAddr = string(buf[offset : offset+domainLen])
			dstPort = int(binary.BigEndian.Uint16(buf[offset+domainLen : offset+domainLen+2]))
			offset += domainLen + 2
		case 0x04: // IPv6
			if n < offset+18 {
				continue
			}
			dstAddr = net.IP(buf[offset : offset+16]).String()
			dstPort = int(binary.BigEndian.Uint16(buf[offset+16 : offset+18]))
			offset += 18
		default:
			continue
		}

		r.addrMu.Lock()
		wasNil := r.clientAddr == nil
		oldAddr := r.clientAddr
		r.clientAddr = srcAddr
		if wasNil {
			go r.flushEarlyReplies()
		}
		r.addrMu.Unlock()
		if oldAddr == nil {
			util.LogInfo("[SOCKS5-SVR] [%s] clientAddr SET: %s (was nil)", r.mapping.Name, srcAddr)
		} else if oldAddr.String() != srcAddr.String() {
			util.LogInfo("[SOCKS5-SVR] [%s] clientAddr CHANGED: %s -> %s", r.mapping.Name, oldAddr, srcAddr)
		} else {
			util.LogDebug("[SOCKS5-SVR] [%s] clientAddr SAME: %s", r.mapping.Name, srcAddr)
		}

		data := buf[offset:n]
		util.LogDebug("[SOCKS5-SVR] [%s] UDP RX from=%s to=%s:%d n=%d", r.mapping.Name, srcAddr, dstAddr, dstPort, len(data))
		req := config.NewConnectRequest(dstAddr, dstPort)
		req = r.ruleConf.Resolving(req)

		proxy := r.ruleConf.Match(req, r.mapping)
		if proxy == nil || strings.ToUpper(proxy.Type) == config.ProxyREJECT {
			continue
		}
		targetKey := fmt.Sprintf("%s:%d", dstAddr, dstPort)
		var isFirst bool
		r.proxyMu.Lock()
		isFirst = r.seenTargets.Put(targetKey)
		r.proxyMu.Unlock()
		if isFirst {
			util.LogInfo("[SOCKS5-SVR] [%s] UDP -> %s:%d via %s(%s)", r.mapping.Name, dstAddr, dstPort, proxy.Name, proxy.Type)
		} else {
			util.LogDebug("[SOCKS5-SVR] [%s] UDP -> %s:%d via %s(%s) %d bytes", r.mapping.Name, dstAddr, dstPort, proxy.Name, proxy.Type, len(data))
		}

		targetAddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(dstAddr, strconv.Itoa(dstPort)))
		if err != nil {
			continue
		}

		// Get or create downstream PacketConn for this proxy
		upc, startGoroutine, err := r.getProxyConn(proxy)
		if err != nil {
			util.LogInfo("[SOCKS5-SVR] [%s] UDP proxy conn fail for %s:%d via %s: %v", r.mapping.Name, dstAddr, dstPort, proxy.Name, err)
			continue
		}
		if startGoroutine != nil {
			startGoroutine()
		}

		r.addrMu.Lock()
		curClientAddr := r.clientAddr
		r.addrMu.Unlock()
		util.LogDebug("[SOCKS5-SVR] [%s] UDP SEND srcClient=%s curClientAddr=%s target=%s via=%s data=%d",
			r.mapping.Name, srcAddr, curClientAddr, targetAddr, proxy.Name, len(data))
		if nw, err := upc.pc.WriteTo(data, targetAddr); err != nil {
			if util.IsClosedErr(err) {
				util.LogDebug("[SOCKS5-SVR] [%s] UDP write closed for %s:%d via %s: %v", r.mapping.Name, dstAddr, dstPort, proxy.Name, err)
			} else {
				util.LogWarn("[SOCKS5-SVR] [%s] UDP write fail for %s:%d via %s: %v", r.mapping.Name, dstAddr, dstPort, proxy.Name, err)
			}
		} else {
			util.LogDebug("[SOCKS5-SVR] [%s] UDP write ok for %s:%d via %s, sent=%d, client=%s", r.mapping.Name, dstAddr, dstPort, proxy.Name, nw, curClientAddr)
		}
	}
}

// getProxyConn returns a cached PacketConn for the proxy, creating one if needed.
// The caller must call the returned start function (if non-nil) after releasing any locks.
func (r *socks5UDPRelay) getProxyConn(proxy *config.Proxy) (*udpProxyConn, func(), error) {
	r.proxyMu.Lock()
	defer r.proxyMu.Unlock()

	if upc, ok := r.proxyConns[proxy.Name]; ok {
		if atomic.LoadInt32(&upc.dead) == 0 {
			return upc, nil, nil
		}
		// proxyReadLoop has exited, remove stale entry and recreate below.
		delete(r.proxyConns, proxy.Name)
		upc.pc.Close()
		util.LogDebug("[SOCKS5-SVR] [%s] UDP stale proxy conn removed for %s, will recreate", r.mapping.Name, proxy.Name)
	}

	pc, err := dialer.NewUDPDialer(proxy).DialPacket()
	if err != nil {
		return nil, nil, err
	}
	util.LogDebug("[SOCKS5-SVR] [%s] UDP DialPacket ok for %s(%s), local=%s", r.mapping.Name, proxy.Name, proxy.Type, pc.LocalAddr())

	ctx, cancel := context.WithCancel(context.Background())
	upc := &udpProxyConn{pc: pc, cancel: cancel}
	r.proxyConns[proxy.Name] = upc

	start := func() {
		go r.proxyReadLoop(ctx, pc, proxy.Name)
	}

	return upc, start, nil
}

// removeProxyConn removes a proxy conn from the cache. Caller must hold proxyMu or
// be in a context where racing with getProxyConn is safe (e.g. proxyReadLoop defer).
func (r *socks5UDPRelay) removeProxyConn(proxyName string) {
	r.proxyMu.Lock()
	delete(r.proxyConns, proxyName)
	r.proxyMu.Unlock()
}

// proxyReadLoop reads replies from a downstream PacketConn and sends them back to the client.
func (r *socks5UDPRelay) proxyReadLoop(ctx context.Context, pc net.PacketConn, proxyName string) {
	defer func() {
		// Mark dead and clean up from cache so getProxyConn will recreate on next use.
		r.proxyMu.Lock()
		if upc, ok := r.proxyConns[proxyName]; ok && upc.pc == pc {
			atomic.StoreInt32(&upc.dead, 1)
			delete(r.proxyConns, proxyName)
		}
		r.proxyMu.Unlock()
		util.LogDebug("[SOCKS5-SVR] [%s] UDP proxyReadLoop exited for %s, cache removed", r.mapping.Name, proxyName)
	}()

	buf := make([]byte, 65535)
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.closed:
			return
		default:
		}

		pc.SetReadDeadline(time.Now().Add(60 * time.Second))
		n, srcAddr, err := pc.ReadFrom(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				util.LogDebug("[SOCKS5-SVR] [%s] UDP reply read timeout from proxy %s", r.mapping.Name, proxyName)
				continue
			}
			if util.IsClosedErr(err) {
				util.LogDebug("[SOCKS5-SVR] [%s] UDP reply read closed from proxy %s (normal): %v", r.mapping.Name, proxyName, err)
			} else {
				util.LogWarn("[SOCKS5-SVR] [%s] UDP reply read error from proxy %s: %v", r.mapping.Name, proxyName, err)
			}
			return
		}
		util.LogDebug("[SOCKS5-SVR] [%s] UDP reply from proxy %s: n=%d src=%s", r.mapping.Name, proxyName, n, srcAddr)
		r.proxyMu.Lock()
		if r.seenTargets.Put(srcAddr.String()) {
			util.LogDebug("[SOCKS5-SVR] [%s] UDP <- %s via %s", r.mapping.Name, srcAddr.String(), proxyName)
		}
		r.proxyMu.Unlock()
		r.addrMu.Lock()
		addr := r.clientAddr
		r.addrMu.Unlock()
		if addr == nil {
			util.LogWarn("[SOCKS5-SVR] [%s] UDP reply DROPPED (no clientAddr yet) from=%s via=%s", r.mapping.Name, srcAddr, proxyName)
			// Buffer reply until clientAddr is known
			r.earlyMu.Lock()
			if len(r.earlyReplies) < maxEarlyReplies {
				packet := make([]byte, n)
				copy(packet, buf[:n])
				r.earlyReplies = append(r.earlyReplies, earlyReply{data: packet, srcAddr: srcAddr})
			}
			r.earlyMu.Unlock()
			continue
		}

		// Build SOCKS5 UDP response header
		header := buildSOCKS5UDPHeader(srcAddr)

		packet := make([]byte, len(header)+n)
		copy(packet, header)
		copy(packet[len(header):], buf[:n])
		if nw, werr := r.clientLn.WriteTo(packet, addr); werr != nil {
			if util.IsClosedErr(werr) {
				util.LogDebug("[SOCKS5-SVR] [%s] UDP reply write closed (normal): client=%s echoSrc=%s", r.mapping.Name, addr, srcAddr)
			} else {
				util.LogWarn("[SOCKS5-SVR] [%s] UDP reply TX error: client=%s echoSrc=%s wrote=%d err=%v", r.mapping.Name, addr, srcAddr, 0, werr)
			}
		} else {
			util.LogDebug("[SOCKS5-SVR] [%s] UDP reply TX: client=%s echoSrc=%s wrote=%d",
				r.mapping.Name, addr, srcAddr, nw)
		}
	}
}

// flushEarlyReplies sends packets buffered before clientAddr was available.
func (r *socks5UDPRelay) flushEarlyReplies() {
	r.earlyMu.Lock()
	early := r.earlyReplies
	r.earlyReplies = nil
	r.earlyMu.Unlock()

	r.addrMu.Lock()
	clientAddr := r.clientAddr
	r.addrMu.Unlock()

	if len(early) == 0 || clientAddr == nil {
		return
	}

	for _, er := range early {
		header := buildSOCKS5UDPHeader(er.srcAddr)
		packet := make([]byte, len(header)+len(er.data))
		copy(packet, header)
		copy(packet[len(header):], er.data)
		if _, werr := r.clientLn.WriteTo(packet, clientAddr); werr != nil {
			if util.IsClosedErr(werr) {
				util.LogDebug("[SOCKS5-SVR] [%s] early reply write closed (normal): %v", r.mapping.Name, werr)
			} else {
				util.LogWarn("[SOCKS5-SVR] [%s] early reply WriteTo error: %v", r.mapping.Name, werr)
			}
		}
	}
}

// buildSOCKS5UDPHeader builds the SOCKS5 UDP reply header for srcAddr.
func buildSOCKS5UDPHeader(srcAddr net.Addr) []byte {
	header := make([]byte, 0, 64)
	header = append(header, 0x00, 0x00, 0x00) // RSV + FRAG

	if srcAddr == nil {
		header = append(header, 0x01, 0, 0, 0, 0, 0, 0)
		return header
	}

	switch addr := srcAddr.(type) {
	case *net.UDPAddr:
		if ip4 := addr.IP.To4(); ip4 != nil {
			header = append(header, 0x01)
			header = append(header, ip4...)
		} else {
			header = append(header, 0x04)
			header = append(header, addr.IP.To16()...)
		}
		portBuf := make([]byte, 2)
		binary.BigEndian.PutUint16(portBuf, uint16(addr.Port))
		header = append(header, portBuf...)
	default:
		s := srcAddr.String()
		host, portStr, _ := net.SplitHostPort(s)
		if host != "" {
			ip := net.ParseIP(host)
			if ip != nil {
				if ip4 := ip.To4(); ip4 != nil {
					header = append(header, 0x01)
					header = append(header, ip4...)
				} else {
					header = append(header, 0x04)
					header = append(header, ip.To16()...)
				}
			} else {
				header = append(header, 0x03, byte(len(host)))
				header = append(header, []byte(host)...)
			}
			port, _ := strconv.Atoi(portStr)
			portBuf := make([]byte, 2)
			binary.BigEndian.PutUint16(portBuf, uint16(port))
			header = append(header, portBuf...)
		} else {
			header = append(header, 0x01, 0, 0, 0, 0, 0, 0)
		}
	}
	return header
}

func (r *socks5UDPRelay) Close() {
	r.closeOnce.Do(func() {
		util.LogInfo("[SOCKS5-SVR] [%s] [%s] UDP ASSOCIATE closing (port %d)", r.mapping.Name, r.clientConn.RemoteAddr(), r.clientLn.LocalAddr().(*net.UDPAddr).Port)
		close(r.closed)
		r.proxyMu.Lock()
		for _, upc := range r.proxyConns {
			upc.cancel()
			upc.pc.Close()
		}
		r.proxyConns = nil
		r.proxyMu.Unlock()
		r.clientLn.Close()
		r.clientConn.Close()
	})
}

func StartSocks5(ruleConf *config.RuleConfiguration, mapping *config.Mapping) (net.Listener, error) {
	srv := &Socks5Server{BaseServer: BaseServer{RuleConf: ruleConf, Mapping: mapping}}
	return startTCP(mapping.Port, srv)
}
