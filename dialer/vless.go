package dialer

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/cnlangzi/proxyclient/xray"
	xnet "github.com/xtls/xray-core/common/net"
	core "github.com/xtls/xray-core/core"

	"phaethon/config"
	"phaethon/util"
)

// VLESSDialer connects through a VLESS proxy using xray-core (supports REALITY/XTLS)
type VLESSDialer struct {
	BaseDialer
	instance *core.Instance
	once     sync.Once
	onceErr  error
}

func (d *VLESSDialer) Dial(dstAddr string, dstPort int) (net.Conn, error) {
	return d.dialWithNetwork(dstAddr, dstPort, xnet.Network_TCP)
}

func (d *VLESSDialer) DialPacket() (net.PacketConn, error) {
	conn, err := d.dialWithNetwork("0.0.0.0", 0, xnet.Network_UDP)
	if err != nil {
		return nil, err
	}
	if pc, ok := conn.(net.PacketConn); ok {
		return pc, nil
	}
	return &vlessPacketConn{Conn: conn}, nil
}

func (d *VLESSDialer) dialWithNetwork(dstAddr string, dstPort int, network xnet.Network) (net.Conn, error) {
	d.once.Do(func() {
		d.instance, d.onceErr = d.startInstance()
	})
	if d.onceErr != nil {
		return nil, fmt.Errorf("vless: start xray instance fail: %w", d.onceErr)
	}

	host := dstAddr
	port := dstPort
	if h, p, err := net.SplitHostPort(dstAddr); err == nil {
		host = h
		if pi, err := strconv.Atoi(p); err == nil {
			port = pi
		}
	}

	dest := xnet.Destination{
		Network: network,
		Address: xnet.ParseAddress(host),
		Port:    xnet.Port(port),
	}

	// core.Dial binds the returned Conn's lifecycle to the supplied context;
	// canceling it after Dial returns closes the pipe. Keep the context alive
	// for the duration of the connection and cancel it when the Conn is closed.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	conn, err := core.Dial(ctx, d.instance, dest)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("vless: dial %s:%d fail: %w", dstAddr, dstPort, err)
	}

	util.LogDebug("[VLESS-CLI] [%s] [%s] Connecting %s:%d via %s:%d", d.Proxy.Name, d.ConnIDStr(), dstAddr, dstPort, d.Proxy.Server, d.Proxy.Port)
	return &vlessConn{Conn: conn, cancel: cancel}, nil
}

// vlessConn wraps a core.Dial connection and cancels its context on Close.
type vlessConn struct {
	net.Conn
	cancel    context.CancelFunc
	closeOnce sync.Once
}

func (c *vlessConn) Close() error {
	c.closeOnce.Do(func() {
		if c.cancel != nil {
			c.cancel()
		}
		_ = c.Conn.Close()
	})
	return nil
}

func (d *VLESSDialer) startInstance() (*core.Instance, error) {
	vlessURI := buildVlessURI(d.Proxy)
	u, err := url.Parse(vlessURI)
	if err != nil {
		return nil, fmt.Errorf("parse vless uri fail: %w", err)
	}
	instance, _, err := xray.StartVless(u, 0)
	return instance, err
}

// vlessPacketConn adapts a xray-core UDP net.Conn to net.PacketConn.
// xray-core UDP connections are destination-specific (dialed to a fixed dest),
// so WriteTo ignores the address parameter and ReadFrom returns the remote addr.
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

func (c *vlessPacketConn) LocalAddr() net.Addr { return c.Conn.LocalAddr() }
func (c *vlessPacketConn) SetDeadline(t time.Time) error {
	return c.Conn.SetDeadline(t)
}
func (c *vlessPacketConn) SetReadDeadline(t time.Time) error {
	return c.Conn.SetReadDeadline(t)
}
func (c *vlessPacketConn) SetWriteDeadline(t time.Time) error {
	return c.Conn.SetWriteDeadline(t)
}

// buildVlessURI converts Proxy config to a standard VLESS URI for xray-core.
// Uses url.URL to handle encoding of UUID/password, IPv6 brackets, and query params correctly.
func buildVlessURI(p *config.Proxy) string {
	uuid := p.UUID
	if uuid == "" {
		uuid = p.Password
	}

	q := make(url.Values)
	q.Set("encryption", "none")

	if p.Flow != "" {
		q.Set("flow", p.Flow)
	}

	security := "tls"
	if p.RealityOpts != nil && p.RealityOpts["public-key"] != "" {
		security = "reality"
	}
	q.Set("security", security)

	sni := p.Sni
	if sni == "" {
		sni = p.Servername
	}
	if sni == "" {
		sni = p.Server
	}
	q.Set("sni", sni)

	if p.RealityOpts != nil {
		if pbk := p.RealityOpts["public-key"]; pbk != "" {
			q.Set("pbk", pbk)
		}
		if sid := p.RealityOpts["short-id"]; sid != "" {
			q.Set("sid", sid)
		}
	}

	if p.Fingerprint != "" {
		q.Set("fp", p.Fingerprint)
	}

	if p.SkipCertVerify {
		q.Set("allowInsecure", "1")
	}

	q.Set("type", "tcp")

	host := net.JoinHostPort(p.Server, strconv.Itoa(p.Port))

	u := &url.URL{
		Scheme:   "vless",
		User:     url.User(uuid),
		Host:     host,
		RawQuery: q.Encode(),
	}
	if p.Name != "" {
		u.Fragment = p.Name
	}

	return u.String()
}
