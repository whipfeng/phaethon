package dialer

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"phaethon/config"
	"phaethon/reverse"
	"phaethon/util"
)

var reverseUDPMu sync.Map

// ReverseDialer obtains a connection from the reverse channel registry
type ReverseDialer struct {
	BaseDialer
}

func (d *ReverseDialer) Dial(dstAddr string, dstPort int) (net.Conn, error) {
	address := d.Proxy.ReverseAddress
	if address == "" {
		address = d.Proxy.Server
	}

	registry := reverse.GlobalRegistry()
	if registry == nil {
		return nil, fmt.Errorf("reverse: registry not initialized")
	}

	conn, err := registry.Match(address)
	if err != nil {
		return nil, fmt.Errorf("reverse: match fail for %s: %w", address, err)
	}

	return reverse.NewReverseFramedConn(conn), nil
}

// DialPacket establishes a UDP channel through the reverse connection.
//
// Architecture (dual-socket):
//   - Port C (targetConn): raw UDP socket for direct communication with target services
//   - Port D (chainConn):  UDP socket through proxy chain (or direct) for encrypted frame exchange
//   - TCP control channel: framed signaling (FrameUDP_CHANNEL), address exchange, session key negotiation, READY signal
//
// Data flow:
//
//	Client -> [SOCKS5] -> Entry -> DialPacket.WriteTo(payload,target) -> encrypt -> chainConn -> ... -> Server
//	Server -> ... -> chainConn -> decrypt -> ReadFrom returns (payload, target) -> [SOCKS5] -> Client
//	Target reply -> targetConn.ReadFrom -> build frame -> encrypt -> chainConn -> ... -> Server -> client
func (d *ReverseDialer) DialPacket() (net.PacketConn, error) {
	address := d.Proxy.ReverseAddress
	if address == "" {
		address = d.Proxy.Server
	}

	muI, _ := reverseUDPMu.LoadOrStore(address, &sync.Mutex{})
	mu := muI.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()

	registry := reverse.GlobalRegistry()
	if registry == nil {
		return nil, fmt.Errorf("reverse-udp: registry not initialized")
	}

	tcpConn, err := registry.Match(address)
	if err != nil {
		return nil, fmt.Errorf("reverse-udp: match fail for %s: %w", address, err)
	}

	// Port C: raw UDP socket to reach target services directly
	targetConn, err := ListenUDP()
	if err != nil {
		tcpConn.Close()
		return nil, fmt.Errorf("reverse-udp: listen target udp fail: %w", err)
	}

	// Port D: UDP socket through proxy chain for encrypted frame exchange.
	var chainConn net.PacketConn
	chainIsDirect := false
	if d.Proxy.Next != nil && d.Proxy.Next.Type != config.ProxyDIRECT {
		chainConn, err = ChainUDPDial(d.Proxy.Next)
		if err != nil {
			targetConn.Close()
			tcpConn.Close()
			return nil, fmt.Errorf("reverse-udp: chain udp dial fail: %w", err)
		}
	} else {
		chainConn = targetConn
		chainIsDirect = true
	}

	// Signal: UDP_CHANNEL mode via frame protocol.
	// Send the literal local address (may be 0.0.0.0/[::]); the server-side
	// handleReverseUDPChannel falls back to tcpConn.RemoteAddr() when the
	// address is unspecified, which yields the correct registry IP from the
	// server's perspective.
	cmdPayload := []byte(chainConn.LocalAddr().String())
	util.LogInfo("[REVERSE-UDP-DIALER] [%s] sending UDP_CHANNEL to server, dialerAddr=%s", d.Proxy.Name, cmdPayload)
	if err := reverse.WriteFrame(tcpConn, reverse.FrameUDPChannel, cmdPayload); err != nil {
		cleanupBoth(targetConn, chainConn, tcpConn, chainIsDirect)
		return nil, fmt.Errorf("reverse-udp: send cmd fail: %w", err)
	}

	// Use buffered reader to safely read line-oriented handshake data.
	// TCP may coalesce multiple small writes into one segment; a naive
	// ReadLine that discards surplus bytes after the first '\n' would lose
	// the remaining lines and deadlock the handshake.
	reader := bufio.NewReader(tcpConn)

	// Receive session key from server (32 bytes, hex-encoded, newline-terminated)
	keyLine, err := reader.ReadString('\n')
	if err != nil {
		cleanupBoth(targetConn, chainConn, tcpConn, chainIsDirect)
		return nil, fmt.Errorf("reverse-udp: read session key fail: %w", err)
	}
	keyLine = strings.TrimRight(keyLine, "\r\n")
	util.LogInfo("[REVERSE-UDP-DIALER] [%s] received session key len=%d", d.Proxy.Name, len(keyLine))
	var sessionKey [32]byte
	keyBytes, hexErr := hex.DecodeString(keyLine)
	if hexErr != nil || len(keyBytes) != 32 {
		cleanupBoth(targetConn, chainConn, tcpConn, chainIsDirect)
		return nil, fmt.Errorf("reverse-udp: invalid session key (hex=%d err=%v)", len(keyLine), hexErr)
	}
	copy(sessionKey[:], keyBytes)
	crypto := util.NewReverseCrypto(sessionKey)

	// Receive server's tunnel local address (newline-terminated).
	addrLine, err := reader.ReadString('\n')
	if err != nil {
		cleanupBoth(targetConn, chainConn, tcpConn, chainIsDirect)
		return nil, fmt.Errorf("reverse-udp: read server addr fail: %w", err)
	}
	addrLine = strings.TrimRight(addrLine, "\r\n")
	util.LogInfo("[REVERSE-UDP-DIALER] [%s] received server tunnel addr=%s", d.Proxy.Name, addrLine)
	remoteAddr, err := net.ResolveUDPAddr("udp", addrLine)
	if err != nil {
		cleanupBoth(targetConn, chainConn, tcpConn, chainIsDirect)
		return nil, fmt.Errorf("reverse-udp: resolve server addr fail: %w", err)
	}

	// Receive READY signal — server confirms channel is established and first heartbeat sent.
	readyLine, err := reader.ReadString('\n')
	if err != nil {
		cleanupBoth(targetConn, chainConn, tcpConn, chainIsDirect)
		return nil, fmt.Errorf("reverse-udp: read READY fail: %w", err)
	}
	readyLine = strings.TrimRight(readyLine, "\r\n")
	util.LogInfo("[REVERSE-UDP-DIALER] [%s] received READY=%q", d.Proxy.Name, readyLine)
	if readyLine != "READY" {
		cleanupBoth(targetConn, chainConn, tcpConn, chainIsDirect)
		return nil, fmt.Errorf("reverse-udp: expected READY, got %q", readyLine)
	}

	// Fallback IP resolution if unspecified
	if remoteAddr.IP == nil || remoteAddr.IP.IsUnspecified() {
		tcpRemote := tcpConn.RemoteAddr().(*net.TCPAddr)
		ip := tcpRemote.IP
		if ip == nil || ip.IsUnspecified() {
			ip = net.ParseIP("127.0.0.1")
		}
		if remoteAddr.Port == 0 {
			remoteAddr.Port = chainConn.LocalAddr().(*net.UDPAddr).Port
		}
		remoteAddr = &net.UDPAddr{IP: ip, Port: remoteAddr.Port}
	}

	pc := &reversePacketConn{
		targetConn:    targetConn,
		chainConn:     chainConn,
		tcpConn:       tcpConn,
		remoteAddr:    remoteAddr,
		crypto:        crypto,
		chainIsDirect: chainIsDirect,
		closed:        make(chan struct{}),
		dataCh:        make(chan readResult, 32),
		readyCh:       make(chan struct{}),
		seenTargets:   util.NewFIFOSet(64),
	}

	go pc.tcpKeepalive()
	go pc.tcpReader()
	go pc.chainReader()
	go pc.udpHeartbeatSender()

	if !chainIsDirect {
		go pc.targetToChain()
	}

	// Wait for first UDP heartbeat to confirm data-plane is alive.
	// The server sends it immediately (not via ticker), so this is fast.
	select {
	case <-pc.readyCh:
	case <-time.After(10 * time.Second):
		cleanupBoth(targetConn, chainConn, tcpConn, chainIsDirect)
		return nil, fmt.Errorf("reverse-udp: no heartbeat from server in 10s")
	case <-pc.closed:
		return nil, fmt.Errorf("reverse-udp: closed while waiting for heartbeat")
	}

	util.LogDebug("[REVERSE-UDP] [%s] channel established, target=%s chain=%s remote=%s key=%x...",
		d.Proxy.Name, targetConn.LocalAddr(), chainConn.LocalAddr(), pc.remoteAddr, sessionKey[:4])
	return pc, nil
}

func cleanupBoth(targetConn, chainConn net.PacketConn, tcpConn net.Conn, chainIsDirect bool) {
	targetConn.Close()
	if !chainIsDirect {
		chainConn.Close()
	}
	tcpConn.Close()
}

type readResult struct {
	targetAddr net.Addr
	payload    []byte
}

// reversePacketConn implements net.PacketConn using encrypted Reverse UDP framing
// over dual sockets through the reverse channel.
type reversePacketConn struct {
	targetConn    net.PacketConn
	chainConn     net.PacketConn
	tcpConn       net.Conn
	remoteAddr    net.Addr
	crypto        *util.ReverseCrypto
	chainIsDirect bool
	closed        chan struct{}
	closeOnce     sync.Once
	readMu        sync.Mutex
	writeMu       sync.Mutex
	addrMu        sync.Mutex
	dataCh        chan readResult
	readyCh       chan struct{} // closed when first UDP heartbeat received (data-plane confirmed)
	authFailCount int
	seenTargets   *util.FIFOSet
}

func (c *reversePacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	for {
		select {
		case <-c.closed:
			return 0, nil, fmt.Errorf("reverse-udp: closed")
		case result := <-c.dataCh:
			nCopied := copy(b, result.payload)
			util.LogDebug("[REVERSE-UDP-DIALER] ReadFrom n=%d target=%s", nCopied, result.targetAddr)
			return nCopied, result.targetAddr, nil
		}
	}
}

// chainReader reads from chainConn (Port D), demuxing:
//   - Heartbeat frames (ATYP=0xFF): update remoteAddr from packet source
//   - Data frames: decrypt -> parse -> send to dataCh
func (c *reversePacketConn) chainReader() {
	buf := make([]byte, 65535)
	defer func() {
		close(c.dataCh)
		c.Close() // ensure full cleanup if we exit unexpectedly
	}()

	for {
		select {
		case <-c.closed:
			return
		default:
		}

		c.chainConn.SetReadDeadline(time.Now().Add(60 * time.Second))
		n, srcAddr, err := c.chainConn.ReadFrom(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			util.LogInfo("[REVERSE-UDP-DIALER] chainReader read err from=%s: %v", c.remoteAddr, err)
			c.Close()
			return
		}

		plaintext, err := c.crypto.Open(buf[:n])
		if err != nil {
			c.authFailCount++
			if c.authFailCount <= 5 || c.authFailCount%100 == 0 {
				util.LogWarn("[REVERSE-UDP-DIALER] auth fail #%d from=%s n=%d err=%v", c.authFailCount, srcAddr, n, err)
			}
			continue
		}

		targetAddr, payload, err := util.ParseReverseUDPFrame(plaintext)
		if err != nil {
			util.LogDebug("[REVERSE-UDP-DIALER] parse fail from=%s n=%d plaintext_len=%d err=%v", srcAddr, n, len(plaintext), err)
			continue
		}

		// Heartbeat (ATYP=0xFF): update remoteAddr from packet source.
		// First heartbeat also unblocks DialPacket's wait (data-plane confirmed).
		if targetAddr == nil && payload == nil {
			util.LogDebug("[REVERSE-UDP-DIALER] heartbeat from=%s", srcAddr)
			c.addrMu.Lock()
			c.remoteAddr = srcAddr
			c.addrMu.Unlock()
			select {
			case <-c.readyCh:
			default:
				close(c.readyCh)
			}
			continue
		}

		if c.seenTargets.Put(targetAddr.String()) {
			util.LogInfo("[REVERSE-UDP-DIALER] UDP <- %s (from=%s n=%d payload=%d)", targetAddr.String(), srcAddr, n, len(payload))
		} else {
			util.LogDebug("[REVERSE-UDP-DIALER] UDP <- %s (from=%s n=%d payload=%d)", targetAddr.String(), srcAddr, n, len(payload))
		}

		p := make([]byte, len(payload))
		copy(p, payload)

		select {
		case c.dataCh <- readResult{targetAddr: targetAddr, payload: p}:
		case <-c.closed:
			return
		}
	}
}

func (c *reversePacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	select {
	case <-c.closed:
		return 0, fmt.Errorf("reverse-udp: closed")
	default:
	}

	frame := util.BuildReverseUDPFrame(addr, b)
	if frame == nil {
		return 0, fmt.Errorf("reverse-udp: build frame fail for addr %v", addr)
	}

	if c.seenTargets.Put(addr.String()) {
		util.LogInfo("[REVERSE-UDP-DIALER] UDP -> %s", addr.String())
	}

	ciphertext := c.crypto.Seal(frame)

	c.addrMu.Lock()
	dstAddr := c.remoteAddr
	c.addrMu.Unlock()
	if dstAddr == nil {
		util.LogError("[REVERSE-UDP-DIALER] WriteTo %s FAILED: remoteAddr is nil", addr)
		return 0, fmt.Errorf("reverse-udp: remoteAddr is nil")
	}
	if _, writeErr := c.chainConn.WriteTo(ciphertext, dstAddr); writeErr != nil {
		util.LogError("[REVERSE-UDP-DIALER] WriteTo %s FAILED: dst=%s err=%v", addr, dstAddr, writeErr)
		return 0, fmt.Errorf("reverse-udp: write fail: %w", writeErr)
	}
	util.LogDebug("[REVERSE-UDP-DIALER] WriteTo %s OK: n=%d dst=%s", addr, len(b), dstAddr)
	return len(b), nil
}

func (c *reversePacketConn) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.targetConn.Close()
		if !c.chainIsDirect {
			c.chainConn.Close()
		}
		c.tcpConn.Close()
	})
	return nil
}

func (c *reversePacketConn) LocalAddr() net.Addr               { return c.targetConn.LocalAddr() }
func (c *reversePacketConn) SetDeadline(t time.Time) error     { return c.chainConn.SetDeadline(t) }
func (c *reversePacketConn) SetReadDeadline(t time.Time) error { return c.chainConn.SetReadDeadline(t) }
func (c *reversePacketConn) SetWriteDeadline(t time.Time) error {
	return c.chainConn.SetWriteDeadline(t)
}

// tcpKeepalive sends HEARTBEAT frames on the TCP control plane.
// Uses Unified Reverse Frame Protocol (FrameHeartbeat type).
// Only sends — the registry side also sends heartbeats independently.
// If write fails, the connection is dead.
func (c *reversePacketConn) tcpKeepalive() {
	const tcpHbInterval = 10 * time.Second
	ticker := time.NewTicker(tcpHbInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.closed:
			return
		case <-ticker.C:
		}
		if err := reverse.WriteFrame(c.tcpConn, reverse.FrameHeartbeat, nil); err != nil {
			c.Close()
			return
		}
	}
}

// tcpReader drains incoming frames from the TCP control connection.
// Prevents TCP receive buffer from filling up when the registry side sends
// heartbeats — without this, the registry WriteMsg blocks on zero-window,
// heartbeats stop, and middlebox idle timers kill the connection.
func (c *reversePacketConn) tcpReader() {
	for {
		select {
		case <-c.closed:
			return
		default:
		}
		c.tcpConn.SetReadDeadline(time.Now().Add(70 * time.Second))
		_, _, err := reverse.ReadFrame(c.tcpConn)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return
		}
		// Heartbeat and any stray frames are silently consumed.
	}
}

// udpHeartbeatSender sends periodic encrypted heartbeat frames to the server's Port D.
// Keeps NAT UDP mappings alive on both sides (3s interval).
func (c *reversePacketConn) udpHeartbeatSender() {
	hbFrame := util.BuildReverseUDPHeartbeat()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-c.closed:
			return
		case <-ticker.C:
		}
		// Double-check closed before sealing/sending — prevents stray
		// heartbeat from an old session reaching a new listener on the
		// same UDP port (port reuse) and causing auth-fail noise.
		select {
		case <-c.closed:
			return
		default:
		}
		ciphertext := c.crypto.Seal(hbFrame)
		c.addrMu.Lock()
		dstAddr := c.remoteAddr
		c.addrMu.Unlock()
		if _, err := c.chainConn.WriteTo(ciphertext, dstAddr); err != nil {
			c.Close()
			return
		}
	}
}

// targetToChain reads raw UDP packets from targetConn (Port C),
// wraps them in encrypted Reverse UDP frames, and sends them via chainConn (Port D).
func (c *reversePacketConn) targetToChain() {
	buf := make([]byte, 65535)
	for {
		select {
		case <-c.closed:
			return
		default:
		}

		c.targetConn.SetReadDeadline(time.Now().Add(60 * time.Second))
		n, srcAddr, err := c.targetConn.ReadFrom(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			if !util.IsClosedErr(err) {
				util.LogWarn("[REVERSE-UDP-DIALER] target read err from=%s: %v", srcAddr, err)
			}
			c.Close()
			return
		}

		frame := util.BuildReverseUDPFrame(srcAddr, buf[:n])
		if frame == nil {
			continue
		}

		ciphertext := c.crypto.Seal(frame)
		c.addrMu.Lock()
		dstAddr := c.remoteAddr
		c.addrMu.Unlock()
		c.writeMu.Lock()
		if _, writeErr := c.chainConn.WriteTo(ciphertext, dstAddr); writeErr != nil {
			c.writeMu.Unlock()
			c.Close()
			return
		}
		c.writeMu.Unlock()
	}
}
