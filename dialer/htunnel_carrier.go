package dialer

import (
	"context"
	"net"

	"phaethon/frame"
	"phaethon/util"
)

// htCarrier is the proxy-level carrier abstraction for h_tunnel. The same
// selection (per proxy, e.g. PHAETHON_HT_WS) drives both normal proxy target
// dialing and the mesh P2P session, so WebSocket is a property of the proxy
// rather than a mesh-only special case.
type htCarrier interface {
	Name() string
	DialTarget(ctx context.Context, host string, port int) (net.Conn, error)
	DialP2P() (frame.FrameTransport, error)
}

// newHTCarrier picks the carrier for a proxy. Default is the original
// HTTP long-poll carrier; PHAETHON_HT_WS opts a proxy into WebSocket.
func newHTCarrier(d *HTunnelDialer) htCarrier {
	if wsEnabledFor(d.Proxy.Name) {
		return &wsCarrier{d: d}
	}
	return &httpCarrier{d: d}
}

type httpCarrier struct{ d *HTunnelDialer }

func (c *httpCarrier) Name() string { return "http" }

func (c *httpCarrier) DialTarget(ctx context.Context, host string, port int) (net.Conn, error) {
	return c.d.dialHTunnel("CONN", host, port)
}

func (c *httpCarrier) DialP2P() (frame.FrameTransport, error) {
	r, err := bindToBase(c.d.Proxy, BindModeP2P, "")
	if err != nil {
		return nil, err
	}
	return r.Frame, nil
}

type wsCarrier struct{ d *HTunnelDialer }

func (c *wsCarrier) Name() string { return "ws" }

func (c *wsCarrier) DialTarget(ctx context.Context, host string, port int) (net.Conn, error) {
	crypto := util.NewHTunnelCrypto(c.d.Proxy.Password)
	return dialWSTarget(ctx, c.d.Proxy, crypto, host, port)
}

func (c *wsCarrier) DialP2P() (frame.FrameTransport, error) {
	crypto := util.NewHTunnelCrypto(c.d.Proxy.Password)
	return dialP2PWebSocket(c.d.Proxy, crypto)
}

// dialHTWithFallback dials the target through the selected carrier, falling back
// to the HTTP carrier when a WS handshake fails (proxy does not allow Upgrade).
func (d *HTunnelDialer) dialHTWithFallback(ctx context.Context, host string, port int) (net.Conn, error) {
	c := newHTCarrier(d)
	conn, err := c.DialTarget(ctx, host, port)
	if err == nil || c.Name() != "ws" {
		return conn, err
	}
	util.LogModuleWarn("htunnel", "websocket target dial failed, falling back to http: proxy=%s err=%v", d.Proxy.Name, err)
	return (&httpCarrier{d: d}).DialTarget(ctx, host, port)
}

// dialP2PWithFallback is the mesh-side counterpart of dialHTWithFallback.
func (d *HTunnelDialer) dialP2PWithFallback() (frame.FrameTransport, error) {
	c := newHTCarrier(d)
	tr, err := c.DialP2P()
	if err == nil || c.Name() != "ws" {
		return tr, err
	}
	util.LogModuleWarn("htunnel", "websocket mesh dial failed, falling back to http: proxy=%s err=%v", d.Proxy.Name, err)
	return (&httpCarrier{d: d}).DialP2P()
}
