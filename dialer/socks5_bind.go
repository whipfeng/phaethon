package dialer

import (
	"context"
	"fmt"

	"phaethon/config"
	"phaethon/frame"
)

// socks5BindStrategy implements BindStrategy for the SOCKS5 protocol.
// Plain SOCKS5 CONNECT/BIND handshake with mode-specific DST.PORT (mirrors
// the server-side reverse.BindPort* semantics so no server changes needed).
type socks5BindStrategy struct{}

func (socks5BindStrategy) Bind(ctx context.Context, p *config.Proxy, mode BindMode, dstAddr string) (*BindResult, error) {
	port := portForMode(mode)
	addr := dstAddr
	// BIND for control and P2P targets the proxy's own server (which IS
	// the registry on the other side). BIND for data uses the caller-
	// supplied dstAddr as the reverse address to validate.
	if mode != BindModeData {
		addr = p.Server
	}

	conn, err := dialToProxyServer(p)
	if err != nil {
		return nil, fmt.Errorf("socks5: connect to %s:%d fail: %w", p.Server, p.Port, err)
	}

	// SOCKS5 BIND: CMD=0x02. dstPort carries the mode.
	if err := socks5Handshake(conn, p, addr, port, 0x02, ""); err != nil {
		conn.Close()
		return nil, err
	}

	r := &BindResult{Conn: conn, Mode: mode}
	if mode == BindModeP2P {
		r.Frame = frame.NewStreamTransport(conn)
	}
	return r, nil
}

func (socks5BindStrategy) SupportsP2P() bool { return true }

func init() {
	RegisterBindStrategy(config.ProxySOCKS5, socks5BindStrategy{})
}
