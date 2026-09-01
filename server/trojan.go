package server

import (
	"context"
	"crypto/tls"
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

// TrojanServer handles Trojan protocol connections
type TrojanServer struct {
	BaseServer
	Password string // SHA224 hex of password
}

func (s *TrojanServer) Serve(listener net.Listener) {
	AcceptLoop(listener, s, "trojan")
}

func (s *TrojanServer) HandleConn(clientConn net.Conn) {
	shouldClose := true
	defer func() {
		if shouldClose {
			clientConn.Close()
		}
	}()

	// Read Trojan request:
	// SHA224(password)[56] + CRLF[2] + CMD[1] + ATYP[1] + DST.ADDR[...] + DST.PORT[2] + CRLF[2]
	passwordBuf := make([]byte, 56)
	if _, err := io.ReadFull(clientConn, passwordBuf); err != nil {
		return
	}
	if string(passwordBuf) != s.Password {
		util.LogInfo("[TROJAN-SVR] [%s] auth fail from %s, expected=%s, got=%s", s.Mapping.Name, clientConn.RemoteAddr(), s.Password, string(passwordBuf))
		return
	}

	// Read CRLF
	crlf := make([]byte, 2)
	if _, err := io.ReadFull(clientConn, crlf); err != nil {
		return
	}
	if crlf[0] != 0x0D || crlf[1] != 0x0A {
		util.LogDebug("[TROJAN-SVR] [%s] invalid CRLF in handshake: %02X %02X", s.Mapping.Name, crlf[0], crlf[1])
		return
	}

	// Read CMD + ATYP
	cmdAtyp := make([]byte, 2)
	if _, err := io.ReadFull(clientConn, cmdAtyp); err != nil {
		return
	}
	cmd := cmdAtyp[0] // 0x01=CONNECT, 0x02=BIND, 0x03=UDP_ASSOCIATE
	atyp := cmdAtyp[1]

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
		return
	}

	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(clientConn, portBuf); err != nil {
		return
	}
	dstPort := int(binary.BigEndian.Uint16(portBuf))

	// Read trailing CRLF
	if _, err := io.ReadFull(clientConn, crlf); err != nil {
		return
	}

	if cmd == 0x02 { // BIND -> reverse registration
		util.LogInfo("[TROJAN-SVR] [%s] BIND received for %s:%d from %s", s.Mapping.Name, dstAddr, dstPort, clientConn.RemoteAddr())
		if dstPort == reverse.BindPortControl {
			// Control connection: PORT=1 means this is a control channel
			handleControlConnection(clientConn, dstAddr)
			shouldClose = false
			return
		}
		if dstPort != reverse.BindPortData {
			util.LogInfo("[TROJAN-SVR] [%s] BIND rejected: invalid port %d (only 0 or 1 allowed)", s.Mapping.Name, dstPort)
			return
		}
		// Data connection: PORT=0 only, goes to Registry
		if !s.RuleConf.HasReverseAddress(dstAddr) {
			// Fallback: check if this is a dynamically allocated address
			if GlobalControlManager == nil || !GlobalControlManager.IsDynamicAddress(dstAddr) {
				return // address not supported, close connection
			}
		}
		reverse.HandleReverseConnection(clientConn, dstAddr)
		shouldClose = false
		return
	}

	if cmd == 0x03 { // UDP ASSOCIATE
		s.handleUDPAssociate(clientConn)
		shouldClose = false
		return
	}

	// Resolve and match
	req := config.NewConnectRequest(dstAddr, dstPort)
	req = s.RuleConf.Resolving(req)

	proxy := s.RuleConf.Match(req, s.Mapping)
	if proxy == nil {
		util.LogInfo("[TROJAN-SVR] [%s] [conn-N/A] all proxies dead, rejecting %s:%d", s.Mapping.Name, req.DstAddr, req.DstPort)
		connlog.Log("Trojan:"+s.Mapping.Name, "TCP", clientConn.RemoteAddr().String(), req.DstAddr, req.DstPort, "", "fail", fmt.Errorf("all proxies dead"))
		return
	}
	if strings.ToUpper(proxy.Type) == config.ProxyREJECT {
		util.LogInfo("[TROJAN-SVR] [%s] [conn-N/A] rejected %s:%d", s.Mapping.Name, req.DstAddr, req.DstPort)
		connlog.Log("Trojan:"+s.Mapping.Name, "TCP", clientConn.RemoteAddr().String(), req.DstAddr, req.DstPort, "", "reject", nil)
		return
	}

	connID := util.NextConnID()
	targetConn, err := dialer.ChainDialWithID(proxy, req.DstAddr, req.DstPort, connID)
	if err != nil {
		util.LogInfo("[TROJAN-SVR] [%s] [%s] connect fail %s:%d: %v", s.Mapping.Name, connID, req.DstAddr, req.DstPort, err)
		connlog.Log("Trojan:"+s.Mapping.Name, "TCP", clientConn.RemoteAddr().String(), req.DstAddr, req.DstPort, proxy.Name, "fail", err)
		return
	}
	defer targetConn.Close()

	util.LogInfo("[TROJAN-SVR] [%s] [%s] %s -> %s:%d via %s(%s)", s.Mapping.Name, connID, clientConn.RemoteAddr(), req.DstAddr, req.DstPort, proxy.Name, proxy.Type)
	connlog.Log("Trojan:"+s.Mapping.Name, "TCP", clientConn.RemoteAddr().String(), req.DstAddr, req.DstPort, proxy.Name, "ok", nil)
	util.RelayWithRateLimit(clientConn, targetConn, proxy.UpRateLimiter, proxy.DownRateLimiter)
}

// handleUDPAssociate handles Trojan UDP ASSOCIATE requests.
func (s *TrojanServer) handleUDPAssociate(tlsConn net.Conn) {
	udpLn, err := dialer.ListenUDP()
	if err != nil {
		util.LogInfo("[TROJAN-SVR] [%s] [%s] UDP listen fail: %v", s.Mapping.Name, tlsConn.RemoteAddr(), err)
		return
	}
	defer udpLn.Close()

	proxyConns := make(map[string]*udpProxyConn)
	seenTargets := util.NewFIFOSet(maxSeenTargets)
	var proxyMu sync.Mutex
	var writeMu sync.Mutex
	closed := make(chan struct{})
	var closeOnce sync.Once

	udpPort := udpLn.LocalAddr().(*net.UDPAddr).Port
	util.LogInfo("[TROJAN-SVR] [%s] [%s] UDP ASSOCIATE started (port %d)", s.Mapping.Name, tlsConn.RemoteAddr(), udpPort)

	closeAll := func() {
		closeOnce.Do(func() {
			util.LogInfo("[TROJAN-SVR] [%s] [%s] UDP ASSOCIATE closed (port %d)", s.Mapping.Name, tlsConn.RemoteAddr(), udpPort)
			close(closed)
			proxyMu.Lock()
			for _, upc := range proxyConns {
				upc.cancel()
				upc.pc.Close()
			}
			proxyConns = nil
			proxyMu.Unlock()
			udpLn.Close()
			tlsConn.Close()
		})
	}
	defer closeAll()

	// Goroutine: read UDP replies and write to TLS connection
	go func() {
		buf := make([]byte, 65535)
		for {
			select {
			case <-closed:
				return
			default:
			}

			udpLn.SetReadDeadline(time.Now().Add(60 * time.Second))
			n, srcAddr, err := udpLn.ReadFrom(buf)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue
				}
				if util.IsClosedErr(err) {
					util.LogDebug("[TROJAN-SVR] [%s] UDP reply read closed (normal): %v", s.Mapping.Name, err)
				} else {
					util.LogWarn("[TROJAN-SVR] [%s] UDP reply read error: %v", s.Mapping.Name, err)
				}
				return
			}

			if seenTargets.Put(srcAddr.String()) {
				util.LogInfo("[TROJAN-SVR] [%s] UDP <- %s", s.Mapping.Name, srcAddr.String())
			}

			atyp, addrBytes := util.EncodeTrojanAddrFromNetAddr(srcAddr)
			var pkt []byte
			pkt = append(pkt, atyp)
			pkt = append(pkt, addrBytes...)
			lengthBuf := make([]byte, 2)
			binary.BigEndian.PutUint16(lengthBuf, uint16(n))
			pkt = append(pkt, lengthBuf...)
			pkt = append(pkt, 0x0D, 0x0A)
			pkt = append(pkt, buf[:n]...)

			writeMu.Lock()
			_, err = tlsConn.Write(pkt)
			writeMu.Unlock()
			if err != nil {
				if util.IsClosedErr(err) {
					util.LogDebug("[TROJAN-SVR] [%s] UDP reply write closed (normal): %v", s.Mapping.Name, err)
				} else {
					util.LogWarn("[TROJAN-SVR] [%s] UDP reply write (target->client) error: %v", s.Mapping.Name, err)
				}
				return
			}
		}
	}()

	// Main loop: read from TLS connection and forward UDP packets
	for {
		select {
		case <-closed:
			return
		default:
		}

		tlsConn.SetReadDeadline(time.Now().Add(60 * time.Second))
		atyp, err := util.ReadByte(tlsConn)
		if err != nil {
			return
		}

		var dstAddr string
		var dstPort int
		switch atyp {
		case 0x01: // IPv4
			ipBuf := make([]byte, 4)
			if _, err := io.ReadFull(tlsConn, ipBuf); err != nil {
				return
			}
			dstAddr = net.IP(ipBuf).String()
			dstPort, err = util.ReadPort(tlsConn)
		case 0x03: // Domain
			lenBuf := make([]byte, 1)
			if _, err := io.ReadFull(tlsConn, lenBuf); err != nil {
				return
			}
			domainBuf := make([]byte, lenBuf[0])
			if _, err := io.ReadFull(tlsConn, domainBuf); err != nil {
				return
			}
			dstAddr = string(domainBuf)
			dstPort, err = util.ReadPort(tlsConn)
		case 0x04: // IPv6
			ipBuf := make([]byte, 16)
			if _, err := io.ReadFull(tlsConn, ipBuf); err != nil {
				return
			}
			dstAddr = net.IP(ipBuf).String()
			dstPort, err = util.ReadPort(tlsConn)
		default:
			return
		}
		if err != nil {
			return
		}

		length, err := util.ReadLength(tlsConn)
		if err != nil {
			return
		}

		crlf := make([]byte, 2)
		if _, err := io.ReadFull(tlsConn, crlf); err != nil {
			return
		}
		if crlf[0] != 0x0D || crlf[1] != 0x0A {
			util.LogDebug("[TROJAN-SVR] [%s] invalid CRLF: %02X %02X", s.Mapping.Name, crlf[0], crlf[1])
			return
		}

		payload := make([]byte, length)
		if _, err := io.ReadFull(tlsConn, payload); err != nil {
			return
		}

		req := config.NewConnectRequest(dstAddr, dstPort)
		req = s.RuleConf.Resolving(req)
		proxy := s.RuleConf.Match(req, s.Mapping)
		if proxy == nil || strings.ToUpper(proxy.Type) == config.ProxyREJECT {
			continue
		}

		targetKey := fmt.Sprintf("%s:%d", req.DstAddr, req.DstPort)
		if seenTargets.Put(targetKey) {
			util.LogInfo("[TROJAN-SVR] [%s] UDP -> %s:%d via %s(%s)", s.Mapping.Name, req.DstAddr, req.DstPort, proxy.Name, proxy.Type)
		}

		targetAddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(req.DstAddr, strconv.Itoa(req.DstPort)))
		if err != nil {
			continue
		}

		if strings.ToUpper(proxy.Type) == config.ProxyDIRECT {
			if _, err := udpLn.WriteTo(payload, targetAddr); err != nil {
				if util.IsClosedErr(err) {
					util.LogDebug("[TROJAN-SVR] [%s] UDP direct write closed for %s:%d: %v", s.Mapping.Name, dstAddr, dstPort, err)
				} else {
					util.LogWarn("[TROJAN-SVR] [%s] UDP direct write fail for %s:%d: %v", s.Mapping.Name, dstAddr, dstPort, err)
				}
			}
		} else {
			var startGoroutine func()
			proxyMu.Lock()
			upc, ok := proxyConns[proxy.Name]
			if !ok {
				pc, err := dialer.NewUDPDialer(proxy).DialPacket()
				if err != nil {
					proxyMu.Unlock()
					util.LogInfo("[TROJAN-SVR] [%s] UDP proxy conn fail for %s:%d via %s: %v", s.Mapping.Name, dstAddr, dstPort, proxy.Name, err)
					continue
				}
				ctx, cancel := context.WithCancel(context.Background())
				upc = &udpProxyConn{pc: pc, cancel: cancel}
				proxyConns[proxy.Name] = upc

				// Defer goroutine start until after lock is released
				startGoroutine = func() {
					go func(pc net.PacketConn) {
						defer func() {
							proxyMu.Lock()
							if upc2, ok := proxyConns[proxy.Name]; ok && upc2.pc == pc {
								atomic.StoreInt32(&upc2.dead, 1)
								delete(proxyConns, proxy.Name)
							}
							proxyMu.Unlock()
						}()
						buf := make([]byte, 65535)
						for {
							select {
							case <-ctx.Done():
								return
							case <-closed:
								return
							default:
							}

							pc.SetReadDeadline(time.Now().Add(60 * time.Second))
							n, srcAddr, err := pc.ReadFrom(buf)
							if err != nil {
								if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
									continue
								}
								return
							}

							if seenTargets.Put(srcAddr.String()) {
								if util.IsClosedErr(err) {
									util.LogDebug("[TROJAN-SVR] [%s] UDP proxy read closed (normal): %v", s.Mapping.Name, err)
								} else {
									util.LogWarn("[TROJAN-SVR] [%s] UDP proxy read error: %v", s.Mapping.Name, err)
								}
								return
							}

							if seenTargets.Put(srcAddr.String()) {
								util.LogInfo("[TROJAN-SVR] [%s] UDP <- %s via %s", s.Mapping.Name, srcAddr.String(), proxy.Name)
							}

							atyp, addrBytes := util.EncodeTrojanAddrFromNetAddr(srcAddr)
							var pkt []byte
							pkt = append(pkt, atyp)
							pkt = append(pkt, addrBytes...)
							lengthBuf := make([]byte, 2)
							binary.BigEndian.PutUint16(lengthBuf, uint16(n))
							pkt = append(pkt, lengthBuf...)
							pkt = append(pkt, 0x0D, 0x0A)
							pkt = append(pkt, buf[:n]...)

							writeMu.Lock()
							_, err = tlsConn.Write(pkt)
							writeMu.Unlock()
							if err != nil {
								if util.IsClosedErr(err) {
									util.LogDebug("[TROJAN-SVR] [%s] UDP proxy write closed (normal): %v", s.Mapping.Name, err)
								} else {
									util.LogWarn("[TROJAN-SVR] [%s] UDP proxy write error: %v", s.Mapping.Name, err)
								}
								return
							}
						}
					}(pc)
				}
			}
			proxyMu.Unlock()

			if startGoroutine != nil {
				startGoroutine()
			}

			if _, err := upc.pc.WriteTo(payload, targetAddr); err != nil {
				if util.IsClosedErr(err) {
					util.LogDebug("[TROJAN-SVR] [%s] UDP write closed for %s:%d via %s: %v", s.Mapping.Name, dstAddr, dstPort, proxy.Name, err)
				} else {
					util.LogWarn("[TROJAN-SVR] [%s] UDP write fail for %s:%d via %s: %v", s.Mapping.Name, dstAddr, dstPort, proxy.Name, err)
				}
			}
		}
	}
}

func StartTrojan(ruleConf *config.RuleConfiguration, mapping *config.Mapping) (net.Listener, error) {
	srv := &TrojanServer{
		BaseServer: BaseServer{RuleConf: ruleConf, Mapping: mapping},
		Password:   util.Sha224Hex(mapping.Password),
	}
	return startTLS(mapping.Port, srv)
}

// StartTrojanSNI starts a Trojan server with SNI-based routing for multiple mappings on the same port
func StartTrojanSNI(ruleConf *config.RuleConfiguration, mappings []*config.Mapping, port int) (net.Listener, error) {
	// Build SNI -> mapping/cert maps
	sniCerts := make(map[string]*tls.Certificate)
	sniMappings := make(map[string]*config.Mapping)
	var defaultCert *tls.Certificate
	var defaultMapping *config.Mapping

	for _, m := range mappings {
		cert, err := util.GenerateSelfSignedCert()
		if err != nil {
			return nil, fmt.Errorf("trojan-sni: generate cert fail: %w", err)
		}
		if m.Sni != "" {
			sniCerts[m.Sni] = &cert
			sniMappings[m.Sni] = m
		} else {
			defaultCert = &cert
			defaultMapping = m
		}
	}

	if defaultCert == nil && len(sniCerts) > 0 {
		for sni, cert := range sniCerts {
			defaultCert = cert
			defaultMapping = sniMappings[sni]
			break
		}
	}

	// Build a single tls.Config with GetConfigForClient for SNI-based cert selection.
	// Certificates is also set as a fallback — GetConfigForClient always returns
	// a per-SNI config in the current implementation, but keeping the fallback
	// makes the code more defensive against future changes.
	var allCerts []tls.Certificate
	for _, cert := range sniCerts {
		allCerts = append(allCerts, *cert)
	}
	if defaultCert != nil {
		allCerts = append(allCerts, *defaultCert)
	}

	tlsConf := &tls.Config{
		Certificates: allCerts,
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			sni := hello.ServerName
			cert, ok := sniCerts[sni]
			if !ok {
				cert = defaultCert
			}
			if cert == nil {
				return nil, fmt.Errorf("no cert for SNI: %s", sni)
			}
			return &tls.Config{
				Certificates: []tls.Certificate{*cert},
			}, nil
		},
	}

	addr := fmt.Sprintf(":%d", port)
	ln, err := tls.Listen("tcp", addr, tlsConf)
	if err != nil {
		return nil, err
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				util.LogInfo("[TROJAN-SNI] accept error: %v", err)
				return
			}
			go func(c net.Conn) {
				tlsConn, ok := c.(*tls.Conn)
				if !ok {
					c.Close()
					return
				}

				// tls.Listen's Accept does NOT complete the handshake.
				// Handshake is deferred until first Read/Write.
				// We must explicitly complete it here so that
				// ConnectionState().ServerName is populated.
				if err := tlsConn.Handshake(); err != nil {
					util.LogInfo("[TROJAN-SNI] handshake error from %s: %v", c.RemoteAddr(), err)
					c.Close()
					return
				}
				util.SetTCPNoDelay(tlsConn)

				sni := tlsConn.ConnectionState().ServerName
				m, ok := sniMappings[sni]
				if !ok {
					m = defaultMapping
				}
				if m == nil {
					util.LogInfo("[TROJAN-SNI] no mapping for SNI=%q from %s", sni, c.RemoteAddr())
					c.Close()
					return
				}

				util.LogInfo("[TROJAN-SNI] SNI=%q mapped to %s for %s", sni, m.Name, c.RemoteAddr())
				srv := &TrojanServer{
					BaseServer: BaseServer{RuleConf: ruleConf, Mapping: m},
					Password:   util.Sha224Hex(m.Password),
				}
				srv.HandleConn(c)
				// HandleConn manages connection lifecycle:
				// - CONNECT: closes clientConn after relay completes
				// - BIND: delegates to reverse.HandleReverseConnection, which manages the connection
			}(conn)
		}
	}()

	return ln, nil
}
