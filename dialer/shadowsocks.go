package dialer

import (
	"encoding/binary"
	"fmt"
	"net"
	"strconv"

	"phaethon/util"

	"github.com/shadowsocks/go-shadowsocks2/core"
)

// ShadowsocksDialer connects through a Shadowsocks server (AEAD only).
type ShadowsocksDialer struct {
	BaseDialer
}

func (d *ShadowsocksDialer) Dial(dstAddr string, dstPort int) (net.Conn, error) {
	cipherName := mapSSCipher(d.Proxy.Cipher)
	if cipherName == "" {
		return nil, fmt.Errorf("ss: unsupported cipher: %s", d.Proxy.Cipher)
	}

	ciph, err := core.PickCipher(cipherName, nil, d.Proxy.Password)
	if err != nil {
		return nil, fmt.Errorf("ss: create cipher fail: %w", err)
	}

	addr := net.JoinHostPort(d.Proxy.Server, strconv.Itoa(d.Proxy.Port))
	conn, err := DialRouteAware("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("ss: connect to server fail: %w", err)
	}
	util.SetTCPNoDelay(conn)

	cConn := ciph.StreamConn(conn)

	// Send target address in shadowsocks format
	ssAddr := buildSSAddr(dstAddr, dstPort)
	if _, err := cConn.Write(ssAddr); err != nil {
		cConn.Close()
		return nil, fmt.Errorf("ss: send target addr fail: %w", err)
	}

	return cConn, nil
}

func (d *ShadowsocksDialer) DialPacket() (net.PacketConn, error) {
	cipherName := mapSSCipher(d.Proxy.Cipher)
	if cipherName == "" {
		return nil, fmt.Errorf("ss: unsupported cipher: %s", d.Proxy.Cipher)
	}

	ciph, err := core.PickCipher(cipherName, nil, d.Proxy.Password)
	if err != nil {
		return nil, fmt.Errorf("ss: create cipher fail: %w", err)
	}

	host := d.Proxy.Server
	serverIP, err := resolveSSServerIP(host)
	if err != nil {
		return nil, fmt.Errorf("ss: resolve server %s fail: %w", host, err)
	}
	addr := net.JoinHostPort(serverIP.String(), strconv.Itoa(d.Proxy.Port))
	srvAddr := &net.UDPAddr{IP: serverIP, Port: d.Proxy.Port}

	pc, err := ListenPacketBoundTo("udp", "", serverIP)
	if err != nil {
		return nil, fmt.Errorf("ss: create udp socket fail: %w", err)
	}

	// Wrap with AEAD encryption
	encPC := ciph.PacketConn(pc)

	connID := util.NextConnID()
	util.LogDebug("[SS-CLI] [%s] [%s] UDP ASSOCIATE via %s", d.Proxy.Name, connID, addr)

	return &shadowsocksPacketConn{
		PacketConn: encPC,
		srvAddr:    srvAddr,
		connID:     connID,
		proxyName:  d.Proxy.Name,
	}, nil
}

// shadowsocksPacketConn wraps an encrypted PacketConn with Shadowsocks UDP addressing.
// WriteTo prepends the SS header with the target address; ReadFrom parses the SS header
// from the decrypted payload to obtain the source address.
type shadowsocksPacketConn struct {
	net.PacketConn
	srvAddr   net.Addr
	rBuf      [65535]byte // pre-allocated read buffer
	connID    string
	proxyName string
}

func (c *shadowsocksPacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	header := buildSSAddrFromNetAddr(addr)
	if header == nil {
		return 0, fmt.Errorf("ss: unsupported address type: %T", addr)
	}
	plaintext := make([]byte, len(header)+len(b))
	copy(plaintext, header)
	copy(plaintext[len(header):], b)
	_, err := c.PacketConn.WriteTo(plaintext, c.srvAddr)
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *shadowsocksPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, _, err := c.PacketConn.ReadFrom(c.rBuf[:])
	if err != nil {
		return 0, nil, err
	}
	srcAddr, data, err := parseSSAddr(c.rBuf[:n])
	if err != nil {
		return 0, nil, err
	}
	n = copy(b, data)
	return n, srcAddr, nil
}

// buildSSAddrFromNetAddr encodes a net.Addr in Shadowsocks request format.
func buildSSAddrFromNetAddr(addr net.Addr) []byte {
	switch a := addr.(type) {
	case *net.UDPAddr:
		return buildSSAddr(a.IP.String(), a.Port)
	case *net.TCPAddr:
		return buildSSAddr(a.IP.String(), a.Port)
	}
	// Try to parse as host:port string
	s := addr.String()
	host, portStr, err := net.SplitHostPort(s)
	if err != nil {
		return nil
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil
	}
	return buildSSAddr(host, port)
}

// parseSSAddr parses a Shadowsocks address header and returns the source address,
// remaining data, and any error.
func parseSSAddr(b []byte) (net.Addr, []byte, error) {
	if len(b) < 1 {
		return nil, nil, fmt.Errorf("ss: empty address header")
	}
	atyp := b[0]
	switch atyp {
	case 0x01: // IPv4
		if len(b) < 1+4+2 {
			return nil, nil, fmt.Errorf("ss: short IPv4 header")
		}
		ip := net.IP(b[1:5])
		port := int(binary.BigEndian.Uint16(b[5:7]))
		return &net.UDPAddr{IP: ip, Port: port}, b[7:], nil
	case 0x03: // Domain
		if len(b) < 2 {
			return nil, nil, fmt.Errorf("ss: short domain header")
		}
		domainLen := int(b[1])
		if len(b) < 2+domainLen+2 {
			return nil, nil, fmt.Errorf("ss: short domain data")
		}
		host := string(b[2 : 2+domainLen])
		port := int(binary.BigEndian.Uint16(b[2+domainLen : 2+domainLen+2]))
		addr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			return nil, nil, err
		}
		return addr, b[2+domainLen+2:], nil
	case 0x04: // IPv6
		if len(b) < 1+16+2 {
			return nil, nil, fmt.Errorf("ss: short IPv6 header")
		}
		ip := net.IP(b[1:17])
		port := int(binary.BigEndian.Uint16(b[17:19]))
		return &net.UDPAddr{IP: ip, Port: port}, b[19:], nil
	default:
		return nil, nil, fmt.Errorf("ss: unknown address type: %d", atyp)
	}
}

func mapSSCipher(cipher string) string {
	switch cipher {
	case "aes-128-gcm":
		return "AEAD_AES_128_GCM"
	case "aes-256-gcm":
		return "AEAD_AES_256_GCM"
	case "chacha20-ietf-poly1305":
		return "AEAD_CHACHA20_POLY1305"
	default:
		return ""
	}
}

// buildSSAddr encodes destination address in shadowsocks request format:
// [ATYP(1)] [DST.ADDR] [DST.PORT(2)]
func buildSSAddr(dstAddr string, dstPort int) []byte {
	ip := net.ParseIP(dstAddr)
	if ip4 := ip.To4(); ip4 != nil {
		addr := make([]byte, 1+4+2)
		addr[0] = 0x01
		copy(addr[1:], ip4)
		binary.BigEndian.PutUint16(addr[5:], uint16(dstPort))
		return addr
	}
	if ip16 := ip.To16(); ip16 != nil {
		addr := make([]byte, 1+16+2)
		addr[0] = 0x04
		copy(addr[1:], ip16)
		binary.BigEndian.PutUint16(addr[17:], uint16(dstPort))
		return addr
	}
	// Domain
	addr := make([]byte, 1+1+len(dstAddr)+2)
	addr[0] = 0x03
	addr[1] = byte(len(dstAddr))
	copy(addr[2:], dstAddr)
	binary.BigEndian.PutUint16(addr[2+len(dstAddr):], uint16(dstPort))
	return addr
}

// resolveSSServerIP resolves the Shadowsocks server address, preferring IPv4.
// When the host is a domain, it uses ResolveRouteAware to bypass the TUN DNS
// hijacker and avoid routing loops.
func resolveSSServerIP(host string) (net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return ip, nil
	}
	ips, err := ResolveRouteAware(host)
	if err != nil {
		return nil, err
	}
	for _, s := range ips {
		if ip := net.ParseIP(s); ip != nil {
			if ip4 := ip.To4(); ip4 != nil {
				return ip4, nil
			}
		}
	}
	for _, s := range ips {
		if ip := net.ParseIP(s); ip != nil {
			return ip, nil
		}
	}
	return nil, fmt.Errorf("no usable IP for %s", host)
}
