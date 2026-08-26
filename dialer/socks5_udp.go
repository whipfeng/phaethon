package dialer

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"phaethon/config"
	"phaethon/util"
)

// Socks5UDPConn implements net.PacketConn over a SOCKS5 UDP relay.
// It wraps raw UDP datagrams with SOCKS5 UDP request/response headers.
// When proxy.Next != nil, it acts as a cascaded UDP relay: WriteTo wraps
// an inner SOCKS5 UDP header, then delegates to the next-hop Socks5UDPConn
// which adds the outer SOCKS5 UDP header.
type Socks5UDPConn struct {
	ctrlConn  net.Conn       // TCP control connection
	udpConn   net.PacketConn // local UDP socket (leaf) or next-hop Socks5UDPConn (cascade)
	relayAddr *net.UDPAddr   // SOCKS5 UDP relay address (destination for udpConn.WriteTo)

	// nextHopRelay is set only in cascaded mode (proxy.Next != nil).
	// It holds the current-level SOCKS5 server's relay address, used as
	// the DST in the outer SOCKS5 UDP header.
	nextHopRelay *net.UDPAddr

	closed    chan struct{}
	closeOnce sync.Once
	readMu    sync.Mutex
	writeMu   sync.Mutex
	rBuf      [65535]byte // pre-allocated read buffer
}

func (c *Socks5UDPConn) ReadFrom(b []byte) (n int, addr net.Addr, err error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()

	for {
		select {
		case <-c.closed:
			return 0, nil, fmt.Errorf("socks5-udp: connection closed")
		default:
		}

		c.udpConn.SetReadDeadline(time.Now().Add(60 * time.Second))
		nr, _, err := c.udpConn.ReadFrom(c.rBuf[:])
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return 0, nil, err
		}

		// Parse SOCKS5 UDP header: RSV(2) + FRAG(1) + ATYP(1) + DST.ADDR + DST.PORT + DATA
		if nr < 10 {
			continue
		}
		if binary.BigEndian.Uint16(c.rBuf[0:2]) != 0 || c.rBuf[2] != 0 {
			continue // not a standard SOCKS5 UDP packet
		}

		atyp := c.rBuf[3]
		offset := 4
		var srcAddr string
		var srcPort int

		switch atyp {
		case 0x01: // IPv4
			if nr < offset+6 {
				continue
			}
			srcAddr = net.IP(c.rBuf[offset : offset+4]).String()
			srcPort = int(binary.BigEndian.Uint16(c.rBuf[offset+4 : offset+6]))
			offset += 6
		case 0x03: // Domain
			if nr < offset+1 {
				continue
			}
			domainLen := int(c.rBuf[offset])
			offset++
			if nr < offset+domainLen+2 {
				continue
			}
			srcAddr = string(c.rBuf[offset : offset+domainLen])
			srcPort = int(binary.BigEndian.Uint16(c.rBuf[offset+domainLen : offset+domainLen+2]))
			offset += domainLen + 2
		case 0x04: // IPv6
			if nr < offset+18 {
				continue
			}
			srcAddr = net.IP(c.rBuf[offset : offset+16]).String()
			srcPort = int(binary.BigEndian.Uint16(c.rBuf[offset+16 : offset+18]))
			offset += 18
		default:
			continue
		}

		dataLen := nr - offset
		if dataLen > len(b) {
			dataLen = len(b)
		}
		copy(b, c.rBuf[offset:offset+dataLen])

		return dataLen, &net.UDPAddr{IP: net.ParseIP(srcAddr), Port: srcPort}, nil
	}
}

func (c *Socks5UDPConn) WriteTo(b []byte, addr net.Addr) (n int, err error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	select {
	case <-c.closed:
		return 0, fmt.Errorf("socks5-udp: connection closed")
	default:
	}

	udpAddr, ok := addr.(*net.UDPAddr)
	if !ok {
		return 0, fmt.Errorf("socks5-udp: invalid address type: %T", addr)
	}

	// Build inner SOCKS5 UDP header: RSV(2)=0 + FRAG(1)=0 + ATYP + DST.ADDR + DST.PORT
	inner := c.buildSocks5UDPHeader(udpAddr)
	inner = append(inner, b...)

	// Cascaded mode: delegate to next-hop Socks5UDPConn.
	// The next-hop will wrap an outer SOCKS5 UDP header with DST=nextHopRelay.
	if c.nextHopRelay != nil {
		_, err = c.udpConn.WriteTo(inner, c.nextHopRelay)
		if err != nil {
			return 0, err
		}
		return len(b), nil
	}

	// Leaf mode: send inner packet directly to the SOCKS5 relay
	_, err = c.udpConn.WriteTo(inner, c.relayAddr)
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

// buildSocks5UDPHeader builds a SOCKS5 UDP header for the given address.
func (c *Socks5UDPConn) buildSocks5UDPHeader(udpAddr *net.UDPAddr) []byte {
	header := make([]byte, 0, 64)
	header = append(header, 0x00, 0x00, 0x00)

	ip := udpAddr.IP
	if ip4 := ip.To4(); ip4 != nil {
		header = append(header, 0x01) // IPv4
		header = append(header, ip4...)
	} else if ip16 := ip.To16(); ip16 != nil {
		header = append(header, 0x04) // IPv6
		header = append(header, ip16...)
	} else {
		// Fallback to domain representation
		s := udpAddr.String()
		host, _, _ := net.SplitHostPort(s)
		if host == "" {
			host = ip.String()
		}
		header = append(header, 0x03)
		header = append(header, byte(len(host)))
		header = append(header, []byte(host)...)
	}

	portBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(portBuf, uint16(udpAddr.Port))
	header = append(header, portBuf...)
	return header
}

func (c *Socks5UDPConn) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.ctrlConn.Close()
		c.udpConn.Close()
	})
	return nil
}

func (c *Socks5UDPConn) LocalAddr() net.Addr {
	return c.udpConn.LocalAddr()
}

func (c *Socks5UDPConn) SetDeadline(t time.Time) error {
	return c.udpConn.SetDeadline(t)
}

func (c *Socks5UDPConn) SetReadDeadline(t time.Time) error {
	return c.udpConn.SetReadDeadline(t)
}

func (c *Socks5UDPConn) SetWriteDeadline(t time.Time) error {
	return c.udpConn.SetWriteDeadline(t)
}

func (c *Socks5UDPConn) keepalive() {
	// RFC 1928: the TCP control connection stays open but silent after UDP
	// ASSOCIATE. We only close the relay when the connection is actually
	// dead (EOF or read error), not on idle timeout.
	buf := make([]byte, 1)
	for {
		select {
		case <-c.closed:
			return
		default:
		}
		if _, err := c.ctrlConn.Read(buf); err != nil {
			if util.IsClosedErr(err) {
				util.LogDebug("[SOCKS5-DIALER] keepalive: ctrlConn closed (normal): %v (ctrl=%s)", err, c.ctrlConn.RemoteAddr())
			} else {
				util.LogInfo("[SOCKS5-DIALER] keepalive: ctrlConn read error=%v, closing relay (ctrl=%s)", err, c.ctrlConn.RemoteAddr())
			}
			c.Close()
			return
		}
	}
}

// Socks5UDPAssociate establishes a SOCKS5 UDP ASSOCIATE relay and returns a PacketConn.
// When proxy.Next != nil, it recursively establishes UDP ASSOCIATE through the
// next-hop proxy, creating a cascaded SOCKS5 UDP relay. The caller is responsible
// for closing the returned PacketConn when done.
func Socks5UDPAssociate(proxy *config.Proxy) (*Socks5UDPConn, error) {
	var nextConn *Socks5UDPConn
	var udpConn net.PacketConn
	var nextRelayAddr *net.UDPAddr

	// 1. If there's a next-hop proxy (and it's not a DIRECT placeholder),
	// recursively establish UDP ASSOCIATE first. This gives us the next-hop's
	// relay address and a PacketConn that can send UDP data through the next-hop.
	// Config setNext() sets Next=SingletonProxy(DIRECT) when no proxy chain is
	// configured; we must skip it to avoid dialing "":0.
	if proxy.Next != nil && proxy.Next.Type != config.ProxyDIRECT {
		var err error
		nextConn, err = Socks5UDPAssociate(proxy.Next)
		if err != nil {
			return nil, fmt.Errorf("socks5-udp: cascade to next proxy %s fail: %w", proxy.Next.Name, err)
		}
		udpConn = nextConn
		nextRelayAddr = nextConn.relayAddr
	}

	// 2. Connect to current SOCKS5 server through the proxy chain (TCP CONNECT tunnel).
	// If proxy.Next != nil, nextDialer.Dial creates a CONNECT tunnel through the
	// next-hop proxy to reach the current SOCKS5 server.
	nextDialer := NewDialer(proxy.Next)
	ctrlConn, err := nextDialer.Dial(proxy.Server, proxy.Port)
	if err != nil {
		if nextConn != nil {
			nextConn.Close()
		}
		return nil, fmt.Errorf("socks5-udp: connect to server %s:%d fail: %w", proxy.Server, proxy.Port, err)
	}

	// 3. SOCKS5 authentication
	if err := socks5Auth(ctrlConn, proxy); err != nil {
		ctrlConn.Close()
		if nextConn != nil {
			nextConn.Close()
		}
		return nil, fmt.Errorf("socks5-udp: %w", err)
	}

	// 4. Send UDP ASSOCIATE request (dst = 0.0.0.0:0)
	req := []byte{0x05, 0x03, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
	if _, err := ctrlConn.Write(req); err != nil {
		ctrlConn.Close()
		if nextConn != nil {
			nextConn.Close()
		}
		return nil, fmt.Errorf("socks5-udp: write associate request fail: %w", err)
	}

	// 5. Read response
	resp := make([]byte, 4)
	if _, err := io.ReadFull(ctrlConn, resp); err != nil {
		ctrlConn.Close()
		if nextConn != nil {
			nextConn.Close()
		}
		return nil, fmt.Errorf("socks5-udp: read response fail: %w", err)
	}
	if resp[0] != 0x05 || resp[1] != 0x00 {
		ctrlConn.Close()
		if nextConn != nil {
			nextConn.Close()
		}
		return nil, fmt.Errorf("socks5-udp: associate failed, version=%d status=%d", resp[0], resp[1])
	}

	// Parse BND.ADDR
	var bndAddr string
	var bndPort int
	switch resp[3] {
	case 0x01: // IPv4
		addrBuf := make([]byte, 4+2)
		if _, err := io.ReadFull(ctrlConn, addrBuf); err != nil {
			ctrlConn.Close()
			if nextConn != nil {
				nextConn.Close()
			}
			return nil, fmt.Errorf("socks5-udp: read bind addr fail: %w", err)
		}
		bndAddr = net.IP(addrBuf[0:4]).String()
		bndPort = int(binary.BigEndian.Uint16(addrBuf[4:6]))
	case 0x03: // Domain
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(ctrlConn, lenBuf); err != nil {
			ctrlConn.Close()
			if nextConn != nil {
				nextConn.Close()
			}
			return nil, fmt.Errorf("socks5-udp: read domain len fail: %w", err)
		}
		domainBuf := make([]byte, int(lenBuf[0])+2)
		if _, err := io.ReadFull(ctrlConn, domainBuf); err != nil {
			ctrlConn.Close()
			if nextConn != nil {
				nextConn.Close()
			}
			return nil, fmt.Errorf("socks5-udp: read domain fail: %w", err)
		}
		bndAddr = string(domainBuf[0:lenBuf[0]])
		bndPort = int(binary.BigEndian.Uint16(domainBuf[lenBuf[0]:]))
	case 0x04: // IPv6
		addrBuf := make([]byte, 16+2)
		if _, err := io.ReadFull(ctrlConn, addrBuf); err != nil {
			ctrlConn.Close()
			if nextConn != nil {
				nextConn.Close()
			}
			return nil, fmt.Errorf("socks5-udp: read ipv6 addr fail: %w", err)
		}
		bndAddr = net.IP(addrBuf[0:16]).String()
		bndPort = int(binary.BigEndian.Uint16(addrBuf[16:18]))
	default:
		ctrlConn.Close()
		if nextConn != nil {
			nextConn.Close()
		}
		return nil, fmt.Errorf("socks5-udp: unknown bind address type: %d", resp[3])
	}

	relayAddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(bndAddr, strconv.Itoa(bndPort)))
	if err != nil {
		ctrlConn.Close()
		if nextConn != nil {
			nextConn.Close()
		}
		return nil, fmt.Errorf("socks5-udp: resolve relay addr fail: %w", err)
	}

	if relayAddr.IP == nil || relayAddr.IP.IsUnspecified() {
		// When connecting through a proxy chain, the UDP relay runs on the
		// SOCKS5 server (proxy.Server), not on the intermediate proxy.
		// Use proxy.Server as the destination IP; fallback to TCP remote
		// address only for direct connections or when Server is a hostname
		// that cannot be resolved.
		ip := net.ParseIP(proxy.Server)
		if ip == nil {
			addrs, err := net.LookupIP(proxy.Server)
			if err == nil && len(addrs) > 0 {
				ip = addrs[0]
			}
		}
		if ip == nil || ip.IsUnspecified() {
			tcpRemote := ctrlConn.RemoteAddr().(*net.TCPAddr)
			ip = tcpRemote.IP
			if ip == nil || ip.IsUnspecified() {
				ip = net.ParseIP("127.0.0.1")
			}
		}
		relayAddr = &net.UDPAddr{IP: ip, Port: relayAddr.Port}
	}

	// 6. Create local UDP socket (only for leaf node).
	// In cascaded mode, udpConn is the next-hop Socks5UDPConn.
	// If udpConn is still nil (no recursion happened, or next-hop was DIRECT),
	// create a local UDP socket.
	if udpConn == nil {
		udpConn, err = ListenPacketRouteAware("udp", "")
		if err != nil {
			ctrlConn.Close()
			return nil, fmt.Errorf("socks5-udp: create udp socket fail: %w", err)
		}
	}

	conn := &Socks5UDPConn{
		ctrlConn: ctrlConn,
		udpConn:  udpConn,
		closed:   make(chan struct{}),
	}

	if proxy.Next != nil {
		// Cascaded: UDP data goes to next-hop's relay; current relay is the DST
		// for the outer SOCKS5 UDP header.
		conn.relayAddr = nextRelayAddr
		conn.nextHopRelay = relayAddr
	} else {
		// Leaf: relay is this server's UDP relay address.
		conn.relayAddr = relayAddr
	}

	// Start TCP keepalive goroutine
	go conn.keepalive()

	if proxy.Next != nil {
		util.LogDebug("[SOCKS5-UDP] [%s] UDP ASSOCIATE cascade ok, nextHop=%s, relay=%s", proxy.Name, relayAddr, nextRelayAddr)
	} else {
		util.LogDebug("[SOCKS5-UDP] [%s] UDP ASSOCIATE ok, relay=%s", proxy.Name, relayAddr)
	}
	return conn, nil
}
