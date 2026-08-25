package dialer

import (
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"phaethon/util"
)

// TrojanDialer connects through a Trojan proxy
type TrojanDialer struct {
	BaseDialer
}

func (d *TrojanDialer) Dial(dstAddr string, dstPort int) (net.Conn, error) {
	// If this proxy is configured as a reverse channel, obtain from registry.
	if conn, err := d.TryReverse(); err != nil {
		return nil, fmt.Errorf("trojan: %w", err)
	} else if conn != nil {
		if err := d.sendTrojanRequest(conn, dstAddr, dstPort); err != nil {
			conn.Close()
			return nil, fmt.Errorf("trojan: reverse request fail: %w", err)
		}
		util.LogDebug("[TROJAN-CLI] [%s] [%s] reverse match ok for %s:%d", d.Proxy.Name, d.ConnIDStr(), dstAddr, dstPort)
		return conn, nil
	}

	// Connect to the next hop
	nextDialer := NewDialer(d.Proxy.Next)
	nextType := "nil"
	if d.Proxy.Next != nil {
		nextType = d.Proxy.Next.Type
	}
	util.LogDebug("[TROJAN-DIAL] [%s] next=%s, dialing %s:%d", d.Proxy.Name, nextType, d.Proxy.Server, d.Proxy.Port)
	conn, err := nextDialer.Dial(d.Proxy.Server, d.Proxy.Port)
	if err != nil {
		return nil, fmt.Errorf("trojan: connect to server %s:%d fail: %w", d.Proxy.Server, d.Proxy.Port, err)
	}

	tlsConn, err := d.TLSHandshake(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	util.SetTCPNoDelay(tlsConn)

	if err := d.sendTrojanRequest(tlsConn, dstAddr, dstPort); err != nil {
		tlsConn.Close()
		return nil, err
	}

	util.LogDebug("[TROJAN-CLI] [%s] [%s] Connecting %s:%d via %s:%d", d.Proxy.Name, d.ConnIDStr(), dstAddr, dstPort, d.Proxy.Server, d.Proxy.Port)
	return tlsConn, nil
}

// DialPacket establishes a UDP tunnel through the Trojan proxy.
func (d *TrojanDialer) DialPacket() (net.PacketConn, error) {
	// Connect to the next hop
	nextDialer := NewDialer(d.Proxy.Next)
	conn, err := nextDialer.Dial(d.Proxy.Server, d.Proxy.Port)
	if err != nil {
		return nil, fmt.Errorf("trojan-udp: connect to server %s:%d fail: %w", d.Proxy.Server, d.Proxy.Port, err)
	}

	tlsConn, err := d.TLSHandshake(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	util.SetTCPNoDelay(tlsConn)

	// Send UDP ASSOCIATE request (CMD=0x03, dst=0.0.0.0:0)
	if err := d.SendTrojanRequestWithCmd(tlsConn, 0x03, "0.0.0.0", 0); err != nil {
		tlsConn.Close()
		return nil, err
	}

	util.LogDebug("[TROJAN-CLI] [%s] [%s] UDP ASSOCIATE via %s:%d", d.Proxy.Name, d.ConnIDStr(), d.Proxy.Server, d.Proxy.Port)

	return &trojanPacketConn{
		tlsConn: tlsConn,
		closed:  make(chan struct{}),
	}, nil
}

// TLSHandshake performs TLS handshake on a plain TCP connection.
func (d *TrojanDialer) TLSHandshake(conn net.Conn) (*tls.Conn, error) {
	tlsConf := &tls.Config{
		InsecureSkipVerify: d.Proxy.SkipCertVerify,
	}
	sni := d.Proxy.Sni
	if sni == "" {
		sni = d.Proxy.Servername
	}
	if sni == "" {
		sni = d.Proxy.Server
	}
	if sni != "" {
		tlsConf.ServerName = sni
	}
	tlsConn := tls.Client(conn, tlsConf)
	if err := tlsConn.Handshake(); err != nil {
		return nil, fmt.Errorf("trojan: TLS handshake fail: %w", err)
	}
	return tlsConn, nil
}

// sendTrojanRequest writes the Trojan request to the connection.
// Format: SHA224(password) + CRLF + CMD + ATYP + DST.ADDR + DST.PORT + CRLF
func (d *TrojanDialer) sendTrojanRequest(conn net.Conn, dstAddr string, dstPort int) error {
	cmd := d.ResolveCmd(dstPort)
	return d.SendTrojanRequestWithCmd(conn, cmd, dstAddr, dstPort)
}

func (d *TrojanDialer) SendTrojanRequestWithCmd(conn net.Conn, cmd byte, dstAddr string, dstPort int) error {
	passwordHash := util.Sha224Hex(d.Proxy.Password)

	atyp, addrBytes, err := util.EncodeTrojanAddr(dstAddr, dstPort)
	if err != nil {
		return err
	}

	var req []byte
	req = append(req, []byte(passwordHash)...) // 56 bytes
	req = append(req, 0x0D, 0x0A)              // CRLF
	req = append(req, cmd)                     // CMD
	req = append(req, atyp)                    // ATYP
	req = append(req, addrBytes...)
	req = append(req, 0x0D, 0x0A) // CRLF

	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("trojan: write request fail: %w", err)
	}
	return nil
}

// trojanPacketConn implements net.PacketConn over a Trojan UDP tunnel.
type trojanPacketConn struct {
	tlsConn net.Conn
	mu      sync.Mutex
	closed  chan struct{}
	once    sync.Once
}

func (c *trojanPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	select {
	case <-c.closed:
		return 0, nil, io.EOF
	default:
	}

	// Read UDP packet: ATYP + DST.ADDR + DST.PORT + LENGTH + CRLF + PAYLOAD
	atyp, err := util.ReadByte(c.tlsConn)
	if err != nil {
		return 0, nil, err
	}

	var addrStr string
	var port int
	switch atyp {
	case 0x01: // IPv4
		ipBuf := make([]byte, 4)
		if _, err := io.ReadFull(c.tlsConn, ipBuf); err != nil {
			return 0, nil, err
		}
		addrStr = net.IP(ipBuf).String()
		port, err = util.ReadPort(c.tlsConn)
		if err != nil {
			return 0, nil, err
		}
	case 0x03: // Domain
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(c.tlsConn, lenBuf); err != nil {
			return 0, nil, err
		}
		domainBuf := make([]byte, lenBuf[0])
		if _, err := io.ReadFull(c.tlsConn, domainBuf); err != nil {
			return 0, nil, err
		}
		addrStr = string(domainBuf)
		port, err = util.ReadPort(c.tlsConn)
		if err != nil {
			return 0, nil, err
		}
	case 0x04: // IPv6
		ipBuf := make([]byte, 16)
		if _, err := io.ReadFull(c.tlsConn, ipBuf); err != nil {
			return 0, nil, err
		}
		addrStr = net.IP(ipBuf).String()
		port, err = util.ReadPort(c.tlsConn)
		if err != nil {
			return 0, nil, err
		}
	default:
		return 0, nil, fmt.Errorf("trojan-udp: unknown ATYP: %d", atyp)
	}

	length, err := util.ReadLength(c.tlsConn)
	if err != nil {
		return 0, nil, err
	}

	// Read CRLF
	crlf := make([]byte, 2)
	if _, err := io.ReadFull(c.tlsConn, crlf); err != nil {
		return 0, nil, err
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(c.tlsConn, payload); err != nil {
		return 0, nil, err
	}

	n := copy(b, payload)
	addr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(addrStr, strconv.Itoa(port)))
	if err != nil {
		return n, &net.UDPAddr{}, nil
	}
	return n, addr, nil
}

func (c *trojanPacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	select {
	case <-c.closed:
		return 0, io.ErrClosedPipe
	default:
	}

	host, portStr, err := net.SplitHostPort(addr.String())
	if err != nil {
		return 0, fmt.Errorf("trojan-udp: invalid addr: %w", err)
	}
	port, _ := strconv.Atoi(portStr)

	atyp, addrBytes, err := util.EncodeTrojanAddr(host, port)
	if err != nil {
		return 0, err
	}

	var pkt []byte
	pkt = append(pkt, atyp)
	pkt = append(pkt, addrBytes...)
	lengthBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(lengthBuf, uint16(len(b)))
	pkt = append(pkt, lengthBuf...)
	pkt = append(pkt, 0x0D, 0x0A) // CRLF
	pkt = append(pkt, b...)

	if _, err := c.tlsConn.Write(pkt); err != nil {
		return 0, fmt.Errorf("trojan-udp: write fail: %w", err)
	}
	return len(b), nil
}

func (c *trojanPacketConn) Close() error {
	c.once.Do(func() {
		close(c.closed)
	})
	return c.tlsConn.Close()
}

func (c *trojanPacketConn) LocalAddr() net.Addr                { return c.tlsConn.LocalAddr() }
func (c *trojanPacketConn) SetDeadline(t time.Time) error      { return c.tlsConn.SetDeadline(t) }
func (c *trojanPacketConn) SetReadDeadline(t time.Time) error  { return c.tlsConn.SetReadDeadline(t) }
func (c *trojanPacketConn) SetWriteDeadline(t time.Time) error { return c.tlsConn.SetWriteDeadline(t) }
