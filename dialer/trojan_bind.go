package dialer

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"

	"phaethon/config"
	"phaethon/frame"
	"phaethon/reverse"
)

// trojanBindStrategy implements BindStrategy for the trojan protocol.
// TLS handshake + Trojan BIND with mode-specific DST.PORT (mirrors the
// server-side reverse.BindPort* semantics so no server changes needed).
type trojanBindStrategy struct{}

func (trojanBindStrategy) Bind(ctx context.Context, p *config.Proxy, mode BindMode, dstAddr string) (*BindResult, error) {
	port := portForMode(mode)
	addr := dstAddr
	// BIND for control and P2P targets the proxy's own server (which IS
	// the registry on the other side). BIND for data uses the caller-
	// supplied dstAddr as the reverse address to validate.
	if mode != BindModeData {
		addr = p.Server
	}

	rawConn, err := dialToProxyServer(p)
	if err != nil {
		return nil, fmt.Errorf("trojan: connect to %s:%d fail: %w", p.Server, p.Port, err)
	}

	tlsConn, err := trojanTLSHandshake(rawConn, p)
	if err != nil {
		rawConn.Close()
		return nil, err
	}

	// Trojan BIND command: CMD=0x02 (BIND). dstPort carries the mode.
	if err := sendTrojanBindRequest(tlsConn, p, 0x02, addr, port); err != nil {
		tlsConn.Close()
		return nil, err
	}

	r := &BindResult{Conn: tlsConn, Mode: mode}
	if mode == BindModeP2P {
		r.Frame = frame.NewStreamTransport(tlsConn)
	}
	return r, nil
}

func (trojanBindStrategy) SupportsP2P() bool { return true }

func init() {
	RegisterBindStrategy(config.ProxyTROJAN, trojanBindStrategy{})
}

// trojanTLSHandshake is the protocol-level TLS bootstrap, identical to
// TrojanDialer.TLSHandshake but decoupled from the dialer receiver so the
// BindStrategy can run without instantiating one.
func trojanTLSHandshake(conn net.Conn, p *config.Proxy) (*tls.Conn, error) {
	tlsConf := &tls.Config{
		InsecureSkipVerify: p.SkipCertVerify,
	}
	sni := p.Sni
	if sni == "" {
		sni = p.Servername
	}
	if sni == "" {
		sni = p.Server
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

// sendTrojanBindRequest is a thin protocol-level shim that mirrors
// TrojanDialer.SendTrojanRequestWithCmd but lives in the bind package.
func sendTrojanBindRequest(conn net.Conn, p *config.Proxy, cmd byte, dstAddr string, dstPort int) error {
	// Reuse the existing method on a throwaway dialer — the receiver is
	// only used for the proxy reference.
	d := &TrojanDialer{BaseDialer: BaseDialer{Proxy: p}}
	return d.SendTrojanRequestWithCmd(conn, cmd, dstAddr, dstPort)
}

// portForMode converts a BindMode to the wire-level DST.PORT value the
// reverse server reads to dispatch control/data/p2p channels.
func portForMode(mode BindMode) int {
	switch mode {
	case BindModeData:
		return reverse.BindPortData
	case BindModeControl:
		return reverse.BindPortControl
	case BindModeP2P:
		return reverse.BindPortP2P
	default:
		return reverse.BindPortData
	}
}
