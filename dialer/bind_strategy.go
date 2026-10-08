package dialer

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"

	"phaethon/config"
	"phaethon/frame"
)

// BindMode selects the BIND semantic for protocol-specific handshakes.
// Values mirror reverse.BindPort{Data,Control,P2P} (0/1/2) so server-side
// protocol handlers that read DST.PORT need no changes.
type BindMode int

const (
	// BindModeData is a reverse/data connection (was BindPortData = 0).
	BindModeData BindMode = 0
	// BindModeControl is a control/listen channel for forwarding
	// (was BindPortControl = 1).
	BindModeControl BindMode = 1
	// BindModeP2P is the mesh P2P transport (was BindPortP2P = 2).
	BindModeP2P BindMode = 2
)

// BindResult is the output of a successful BIND handshake. Conn is always
// set; Frame is set only when mode == BindModeP2P and the protocol supports
// framing.
type BindResult struct {
	Conn  net.Conn
	Frame frame.FrameTransport
	Mode  BindMode
}

// BindStrategy is the per-protocol BIND handshake implementation.
//
// Each proxy type registers a BindStrategy that knows how to:
//   - Open a connection to the proxy server (through the via-chain if any)
//   - Perform the protocol-specific BIND handshake at the requested mode
//   - For BindModeP2P, wrap the connection in a frame.FrameTransport so the
//     mesh P2P stack can run on top
//
// The dstAddr parameter is used only by BindModeData (reverse) — it is the
// reverse address sent in the BIND request for server-side validation.
// For BindModeControl / BindModeP2P, dstAddr is ignored (the proxy's own
// server is used as the BIND target).
type BindStrategy interface {
	// Bind dials the proxy and performs the BIND handshake.
	Bind(ctx context.Context, proxy *config.Proxy, mode BindMode, dstAddr string) (*BindResult, error)

	// SupportsP2P returns true if the protocol has a v2 (P2P) variant.
	// If false, Bind with BindModeP2P must return an error.
	SupportsP2P() bool
}

// Registry. Each protocol package (trojan_bind.go, socks5_bind.go,
// htunnel_bind.go, ...) calls RegisterBindStrategy from its init().
var (
	bindMu        sync.RWMutex
	bindRegistry  = map[string]BindStrategy{}
)

// RegisterBindStrategy registers a BindStrategy for a proxy type. The
// proxy type name is normalized to upper-case so lookups are case-insensitive.
func RegisterBindStrategy(proxyType string, s BindStrategy) {
	bindMu.Lock()
	defer bindMu.Unlock()
	bindRegistry[strings.ToUpper(proxyType)] = s
}

// GetBindStrategy returns the strategy for a proxy type, or false if none
// is registered.
func GetBindStrategy(proxyType string) (BindStrategy, bool) {
	bindMu.RLock()
	defer bindMu.RUnlock()
	s, ok := bindRegistry[strings.ToUpper(proxyType)]
	return s, ok
}

// bindToBase is a small helper for the per-dialer DialControl/DialP2P/DialReverse
// wrappers: it pulls a strategy and calls Bind with a background context.
func bindToBase(proxy *config.Proxy, mode BindMode, dstAddr string) (*BindResult, error) {
	s, ok := GetBindStrategy(proxy.Type)
	if !ok {
		return nil, fmt.Errorf("dialer: no BindStrategy registered for proxy type %q", proxy.Type)
	}
	return s.Bind(context.Background(), proxy, mode, dstAddr)
}

// dialToProxyServer is shared by trojan/socks5/htunnel BindStrategy
// implementations: opens a TCP (or chain-routed) connection to proxy.Server.
// Honors the via-chain (proxy.Next).
func dialToProxyServer(proxy *config.Proxy) (net.Conn, error) {
	d := NewDialer(proxy.Next)
	return d.Dial(proxy.Server, proxy.Port)
}
