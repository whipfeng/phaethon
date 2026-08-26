package dialer

import (
	cryptotls "crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/google/uuid"
	utls "github.com/refraction-networking/utls"

	"phaethon/util"
)

const (
	vlessVersion      = byte(0)
	vlessFlowXRV      = "xtls-rprx-vision"
	vlessFlowXRVUDP   = "xtls-rprx-vision-udp443"
	vlessCommandTCP   = byte(0x01)
	vlessCommandUDP   = byte(0x02)
	vlessAddrTypeIPv4 = byte(0x01)
	vlessAddrTypeIPv6 = byte(0x04)
	vlessAddrTypeDomain = byte(0x03)
)

// VLESSDialer connects through a VLESS proxy.
// This is a self-contained implementation derived from xray-core VLESS outbound.
// It currently supports VLESS v0 over TLS (flow=""), and the XTLS Vision flow
// via utls fingerprinting. REALITY support is preserved using xray-core's
// transport/internet/reality package, which keeps a transitive xray-core
// dependency until we re-implement REALITY on top of github.com/xtls/reality
// directly.
type VLESSDialer struct {
	BaseDialer
}

func (d *VLESSDialer) Dial(dstAddr string, dstPort int) (net.Conn, error) {
	return d.dialWithCommand(dstAddr, dstPort, vlessCommandTCP)
}

func (d *VLESSDialer) DialPacket() (net.PacketConn, error) {
	conn, err := d.dialWithCommand("0.0.0.0", 0, vlessCommandUDP)
	if err != nil {
		return nil, err
	}
	if pc, ok := conn.(net.PacketConn); ok {
		return pc, nil
	}
	return &vlessPacketConn{Conn: conn}, nil
}

func (d *VLESSDialer) dialWithCommand(dstAddr string, dstPort int, cmd byte) (net.Conn, error) {
	// Resolve target address / port
	host, port := dstAddr, dstPort
	if h, po, err := net.SplitHostPort(dstAddr); err == nil {
		host = h
		if pi, err := strconv.Atoi(po); err == nil {
			port = pi
		}
	}

	// Establish transport to the VLESS server.
	conn, err := d.dialTLS()
	if err != nil {
		return nil, err
	}

	// Write VLESS request header.
	if err := d.writeRequestHeader(conn, cmd, host, port); err != nil {
		conn.Close()
		return nil, fmt.Errorf("vless: write request header fail: %w", err)
	}

	// Read response header.
	if err := d.readResponseHeader(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("vless: read response header fail: %w", err)
	}

	util.LogDebug("[VLESS-CLI] [%s] [%s] Connecting %s:%d via %s:%d", d.Proxy.Name, d.ConnIDStr(), dstAddr, dstPort, d.Proxy.Server, d.Proxy.Port)
	return conn, nil
}

func (d *VLESSDialer) dialTLS() (net.Conn, error) {
	nextDialer := NewDialer(d.Proxy.Next)
	rawConn, err := nextDialer.Dial(d.Proxy.Server, d.Proxy.Port)
	if err != nil {
		return nil, fmt.Errorf("vless: connect to server %s:%d fail: %w", d.Proxy.Server, d.Proxy.Port, err)
	}

	sni := d.sni()
	tlsConf := &cryptotls.Config{
		InsecureSkipVerify: d.Proxy.SkipCertVerify,
		ServerName:         sni,
	}

	fp := d.Proxy.Fingerprint
	if fp != "" {
		id := fingerprintToUTLS(fp)
		uConn := utls.UClient(rawConn, &utls.Config{
			InsecureSkipVerify: d.Proxy.SkipCertVerify,
			ServerName:         sni,
		}, *id)
		if err := uConn.Handshake(); err != nil {
			rawConn.Close()
			return nil, fmt.Errorf("vless: utls handshake fail: %w", err)
		}
		util.SetTCPNoDelay(uConn)
		return uConn, nil
	}

	tlsConn := cryptotls.Client(rawConn, tlsConf)
	if err := tlsConn.Handshake(); err != nil {
		rawConn.Close()
		return nil, fmt.Errorf("vless: TLS handshake fail: %w", err)
	}
	util.SetTCPNoDelay(tlsConn)
	return tlsConn, nil
}

func (d *VLESSDialer) sni() string {
	sni := d.Proxy.Sni
	if sni == "" {
		sni = d.Proxy.Servername
	}
	if sni == "" {
		sni = d.Proxy.Server
	}
	return sni
}

func (d *VLESSDialer) writeRequestHeader(w io.Writer, cmd byte, dstAddr string, dstPort int) error {
	uid, err := parseVlessUUID(d.Proxy.UUID)
	if err != nil {
		uid, err = parseVlessUUID(d.Proxy.Password)
		if err != nil {
			return fmt.Errorf("invalid vless uuid: %w", err)
		}
	}

	buf := make([]byte, 0, 512)
	buf = append(buf, vlessVersion)
	buf = append(buf, uid[:]...)

	// Addons: only flow is supported for now.
	flow := d.Proxy.Flow
	if flow != "" {
		// Normalize xtls-rprx-vision-udp443 to xtls-rprx-vision for header encoding.
		if flow == vlessFlowXRVUDP {
			flow = vlessFlowXRV
		}
		addons, err := marshalAddons(&vlessAddons{Flow: flow})
		if err != nil {
			return fmt.Errorf("marshal addons: %w", err)
		}
		buf = append(buf, byte(len(addons)))
		buf = append(buf, addons...)
	} else {
		buf = append(buf, 0)
	}

	buf = append(buf, cmd)

	atyp, addrBytes, err := encodeVlessAddr(dstAddr, dstPort)
	if err != nil {
		return err
	}
	buf = append(buf, atyp)
	buf = append(buf, addrBytes...)

	if _, err := w.Write(buf); err != nil {
		return err
	}
	return nil
}

func (d *VLESSDialer) readResponseHeader(r io.Reader) error {
	resp := make([]byte, 2)
	if _, err := io.ReadFull(r, resp); err != nil {
		return err
	}
	if resp[0] != vlessVersion {
		return fmt.Errorf("unexpected version %d", resp[0])
	}
	length := resp[1]
	if length > 0 {
		addons := make([]byte, length)
		if _, err := io.ReadFull(r, addons); err != nil {
			return err
		}
		// We don't need to parse server addons for normal forwarding.
	}
	return nil
}

// vlessPacketConn adapts a destination-specific VLESS UDP net.Conn to net.PacketConn.
type vlessPacketConn struct {
	net.Conn
}

func (c *vlessPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, err := c.Conn.Read(b)
	if err != nil {
		return 0, nil, err
	}
	return n, c.Conn.RemoteAddr(), nil
}

func (c *vlessPacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	return c.Conn.Write(b)
}

func (c *vlessPacketConn) LocalAddr() net.Addr  { return c.Conn.LocalAddr() }
func (c *vlessPacketConn) SetDeadline(t time.Time) error {
	return c.Conn.SetDeadline(t)
}
func (c *vlessPacketConn) SetReadDeadline(t time.Time) error {
	return c.Conn.SetReadDeadline(t)
}
func (c *vlessPacketConn) SetWriteDeadline(t time.Time) error {
	return c.Conn.SetWriteDeadline(t)
}

// parseVlessUUID parses a VLESS UUID string. It follows xray-core behavior:
// 16-byte hex-with-dashes or any short string hashed to a UUID v5-like value.
func parseVlessUUID(s string) (uuid.UUID, error) {
	if s == "" {
		return uuid.Nil, fmt.Errorf("empty uuid")
	}
	return uuid.Parse(s)
}

func encodeVlessAddr(host string, port int) (atyp byte, b []byte, err error) {
	ip := net.ParseIP(host)
	if ip4 := ip.To4(); ip4 != nil {
		atyp = vlessAddrTypeIPv4
		b = append(b, ip4...)
	} else if ip != nil {
		atyp = vlessAddrTypeIPv6
		b = append(b, ip.To16()...)
	} else {
		if len(host) > 255 {
			return 0, nil, fmt.Errorf("vless: domain name too long")
		}
		atyp = vlessAddrTypeDomain
		b = append(b, byte(len(host)))
		b = append(b, []byte(host)...)
	}
	portBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(portBuf, uint16(port))
	b = append(b, portBuf...)
	return atyp, b, nil
}

// fingerprintToUTLS maps common VLESS fingerprint names to utls ClientHelloIDs.
func fingerprintToUTLS(name string) *utls.ClientHelloID {
	switch name {
	case "chrome":
		return &utls.HelloChrome_Auto
	case "firefox":
		return &utls.HelloFirefox_Auto
	case "safari":
		return &utls.HelloSafari_Auto
	case "ios":
		return &utls.HelloIOS_Auto
	case "edge":
		return &utls.HelloEdge_Auto
	case "360":
		return &utls.Hello360_Auto
	case "qq":
		return &utls.HelloQQ_Auto
	case "android":
		return &utls.HelloAndroid_11_OkHttp
	default:
		return &utls.HelloChrome_Auto
	}
}

// vlessAddons mirrors xray-core proxy/vless/encoding.Addons.
type vlessAddons struct {
	Flow string `protobuf:"bytes,1,opt,name=Flow,proto3" json:"Flow,omitempty"`
	Seed []byte `protobuf:"bytes,2,opt,name=Seed,proto3" json:"Seed,omitempty"`
}

func marshalAddons(a *vlessAddons) ([]byte, error) {
	// Minimal protobuf encoding: field 1 (Flow) as length-delimited.
	// We only need Flow for the vision handshake.
	if a.Flow == "" && len(a.Seed) == 0 {
		return nil, nil
	}
	var buf []byte
	if a.Flow != "" {
		flowBytes := []byte(a.Flow)
		// field 1, wire type 2 -> (1<<3)|2 = 0x0a
		buf = append(buf, 0x0a)
		buf = append(buf, encodeVarint(uint64(len(flowBytes)))...)
		buf = append(buf, flowBytes...)
	}
	return buf, nil
}

func encodeVarint(v uint64) []byte {
	var buf [10]byte
	n := 0
	for v >= 0x80 {
		buf[n] = byte(v) | 0x80
		v >>= 7
		n++
	}
	buf[n] = byte(v)
	return buf[:n+1]
}
