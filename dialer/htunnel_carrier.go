package dialer

import (
	"context"
	"fmt"
	"net"

	"phaethon/config"
	"phaethon/frame"
	"phaethon/reverse"
	"phaethon/util"
)

// htCarrier is the carrier implementation behind the h_tunnel BindStrategy:
// the same per-proxy selection (PHAETHON_HT_WS) drives normal target dialing
// and the P2P/MESH handshake, so WebSocket is a polymorphic carrier rather
// than a mesh-only special case.
type htCarrier interface {
	Name() string
	DialTarget(ctx context.Context, host string, port int) (net.Conn, error)
	DialP2P() (frame.FrameTransport, error)
	// DialBIND opens a reverse/data or control channel (BindModeData /
	// BindModeControl). P2P sessions use DialP2P instead so that the WS
	// carrier can run them as persistent frame transports.
	DialBIND(ctx context.Context, mode BindMode, dstAddr string) (net.Conn, error)
}

// newHTCarrier picks the carrier for a proxy. Default is try-WS-first with an
// HTTP fallback on handshake failure; PHAETHON_HT_WS=0 forces HTTP, while a
// comma-separated proxy-name list restricts WS to those proxies.
func newHTCarrier(p *config.Proxy) htCarrier {
	if wsEnabledFor(p.Name) {
		return &wsCarrier{p: p}
	}
	return &httpCarrier{p: p}
}

type httpCarrier struct{ p *config.Proxy }

func (c *httpCarrier) Name() string { return "http" }

func (c *httpCarrier) DialTarget(ctx context.Context, host string, port int) (net.Conn, error) {
	d := &HTunnelDialer{BaseDialer: BaseDialer{Proxy: c.p}}
	return d.dialHTunnel("CONN", host, port)
}

func (c *httpCarrier) DialP2P() (frame.FrameTransport, error) {
	return dialHTP2PHTTP(c.p)
}

func (c *httpCarrier) DialBIND(ctx context.Context, mode BindMode, dstAddr string) (net.Conn, error) {
	switch mode {
	case BindModeData:
		return dialHTunnelBIND(c.p, dstAddr, reverse.BindPortData)
	case BindModeControl:
		return dialHTunnelBIND(c.p, c.p.Server, reverse.BindPortControl)
	default:
		return nil, fmt.Errorf("htunnel-http: unsupported BIND mode %d", mode)
	}
}

type wsCarrier struct{ p *config.Proxy }

func (c *wsCarrier) Name() string { return "ws" }

func (c *wsCarrier) DialTarget(ctx context.Context, host string, port int) (net.Conn, error) {
	crypto := util.NewHTunnelCrypto(c.p.Password)
	return dialWSTarget(ctx, c.p, crypto, host, port)
}

func (c *wsCarrier) DialP2P() (frame.FrameTransport, error) {
	crypto := util.NewHTunnelCrypto(c.p.Password)
	return dialP2PWebSocket(c.p, crypto)
}

func (c *wsCarrier) DialBIND(ctx context.Context, mode BindMode, dstAddr string) (net.Conn, error) {
	crypto := util.NewHTunnelCrypto(c.p.Password)
	switch mode {
	case BindModeData:
		return dialWSBind(ctx, c.p, crypto, dstAddr, reverse.BindPortData)
	case BindModeControl:
		return dialWSBind(ctx, c.p, crypto, c.p.Server, reverse.BindPortControl)
	default:
		return nil, fmt.Errorf("htunnel-ws: unsupported BIND mode %d", mode)
	}
}

// dialHTP2PHTTP is the pure-HTTP P2P handshake: v2 MESH direct when configured,
// otherwise the v1 BIND stream wrapped as a frame transport.
func dialHTP2PHTTP(p *config.Proxy) (frame.FrameTransport, error) {
	if p.IsP2P() && p.V2 {
		return dialHTunnelMeshDirect(p)
	}
	conn, err := dialHTunnelBIND(p, p.Server, reverse.BindPortP2P)
	if err != nil {
		return nil, err
	}
	return frame.NewStreamTransport(conn), nil
}

// dialHTP2PWithFallback selects the carrier for the P2P handshake, falling
// back to HTTP when the WebSocket handshake fails.
func dialHTP2PWithFallback(p *config.Proxy) (frame.FrameTransport, error) {
	c := newHTCarrier(p)
	tr, err := c.DialP2P()
	if err == nil || c.Name() != "ws" {
		return tr, err
	}
	markHTCarrierResult(p.Name, false)
	util.LogModuleWarn("htunnel", "websocket mesh handshake failed, falling back to http: proxy=%s err=%v", p.Name, err)
	return dialHTP2PHTTP(p)
}

// dialHTWithFallback is the normal-target counterpart of dialHTP2PWithFallback.
func (d *HTunnelDialer) dialHTWithFallback(ctx context.Context, host string, port int) (net.Conn, error) {
	c := newHTCarrier(d.Proxy)
	conn, err := c.DialTarget(ctx, host, port)
	if err == nil || c.Name() != "ws" {
		return conn, err
	}
	util.LogModuleWarn("htunnel", "websocket target dial failed, falling back to http: proxy=%s err=%v", d.Proxy.Name, err)
	markHTCarrierResult(d.Proxy.Name, false)
	return (&httpCarrier{p: d.Proxy}).DialTarget(ctx, host, port)
}

// dialHTBINDWithFallback is the BIND-channel counterpart: WS when the proxy
// carrier supports it, HTTP otherwise (including WS handshake failure).
func dialHTBINDWithFallback(ctx context.Context, p *config.Proxy, mode BindMode, dstAddr string) (net.Conn, error) {
	c := newHTCarrier(p)
	conn, err := c.DialBIND(ctx, mode, dstAddr)
	if err == nil || c.Name() != "ws" {
		return conn, err
	}
	util.LogModuleWarn("htunnel", "websocket bind dial failed, falling back to http: proxy=%s mode=%d err=%v", p.Name, mode, err)
	markHTCarrierResult(p.Name, false)
	return (&httpCarrier{p: p}).DialBIND(ctx, mode, dstAddr)
}
