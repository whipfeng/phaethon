package dialer

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"

	"phaethon/config"
	"phaethon/frame"
	"phaethon/util"
)

// wsEnabledFor reports whether the WebSocket carrier is enabled for a proxy.
// PHAETHON_HT_WS accepts "1"/"all"/"auto" or a comma-separated proxy-name list
// (e.g. "MB_HT"). Default off.
func wsEnabledFor(proxyName string) bool {
	v := strings.TrimSpace(os.Getenv("PHAETHON_HT_WS"))
	if v == "" {
		return false
	}
	if v == "1" || strings.EqualFold(v, "all") || strings.EqualFold(v, "auto") {
		return true
	}
	for _, n := range strings.Split(v, ",") {
		if strings.TrimSpace(n) == proxyName {
			return true
		}
	}
	return false
}

// wsURLFor converts the proxy's HTTP URL into a WebSocket URL (fresh connID
// path suffix) and the matching Origin header value.
func wsURLFor(proxy *config.Proxy) (*url.URL, string, error) {
	u, err := url.Parse(proxy.URL)
	if err != nil {
		return nil, "", fmt.Errorf("htunnel-ws: bad url: %w", err)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "ws", "wss":
	default:
		return nil, "", fmt.Errorf("htunnel-ws: unsupported scheme %q", u.Scheme)
	}
	origin := "http://" + u.Host
	if u.Scheme == "wss" {
		origin = "https://" + u.Host
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/ws/" + util.NextConnID()
	return u, origin, nil
}

// dialWSConn opens the TCP connection to the WebSocket server, honoring the
// proxy chain (`via`) exactly like the HTTP long-poll carrier. Dialing direct
// here used to bypass `via`, so an h_tunnel link with via=SSH (target on the
// via side's LAN, e.g. 10.140.51.2:32457) timed out.
func dialWSConn(ctx context.Context, proxy *config.Proxy, u *url.URL) (net.Conn, error) {
	host := u.Hostname()
	port := 80
	if u.Scheme == "wss" {
		port = 443
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			port = n
		}
	}
	if proxy.Next != nil && !strings.EqualFold(proxy.Next.Type, config.ProxyDIRECT) {
		return dialWithContext(ctx, NewDialer(proxy.Next), host, port)
	}
	return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
}

// htWSCarrierState records the last observed WebSocket outcome per proxy so
// the admin console can show which carrier is actually in use.
var htWSCarrierState sync.Map // proxy name -> *htWSRecord

type htWSRecord struct {
	mu   sync.Mutex
	seen bool
	ok   bool
}

func markHTCarrierResult(proxyName string, ok bool) {
	if proxyName == "" {
		return
	}
	v, _ := htWSCarrierState.LoadOrStore(proxyName, &htWSRecord{})
	rec := v.(*htWSRecord)
	rec.mu.Lock()
	rec.seen, rec.ok = true, ok
	rec.mu.Unlock()
}

// HTCarrierLabel returns the h_tunnel carrier label shown in the console:
//
//	""              not an h_tunnel proxy
//	"WS"            WebSocket enabled (no handshake yet or last one succeeded)
//	"HTTP (WS 回退)" WebSocket enabled but the last handshake failed
//	"HTTP"          WebSocket not enabled for this proxy
func HTCarrierLabel(p *config.Proxy) string {
	if p == nil || !strings.EqualFold(p.Type, config.ProxyH_TUNNEL) {
		return ""
	}
	if !wsEnabledFor(p.Name) {
		return "HTTP"
	}
	if v, ok := htWSCarrierState.Load(p.Name); ok {
		rec := v.(*htWSRecord)
		rec.mu.Lock()
		seen, wsOK := rec.seen, rec.ok
		rec.mu.Unlock()
		if seen && !wsOK {
			return "HTTP (WS 回退)"
		}
	}
	return "WS"
}

// dialWSBind opens a WebSocket carrying one BIND channel (reverse data or
// control). It uses the same encrypted X-H/X-P headers as the HTTP carrier,
// with X-C: BIND selecting the bind semantics on the server, and returns a
// net.Conn byte stream like the HTTP three-step BIND conn.
func dialWSBind(ctx context.Context, proxy *config.Proxy, crypto *util.HTunnelCrypto, dstAddr string, bindPort int) (net.Conn, error) {
	u, origin, err := wsURLFor(proxy)
	if err != nil {
		return nil, err
	}
	cfg, err := websocket.NewConfig(u.String(), origin)
	if err != nil {
		return nil, fmt.Errorf("htunnel-ws: config: %w", err)
	}
	cfg.Header.Set(HeaderCommand, "BIND")
	cfg.Header.Set(HeaderTargetHost, crypto.SealHeader(dstAddr))
	cfg.Header.Set(HeaderTargetPort, crypto.SealHeader(strconv.Itoa(bindPort)))

	conn, err := dialWSConn(ctx, proxy, u)
	if err != nil {
		return nil, fmt.Errorf("htunnel-ws: dial fail: %w", err)
	}
	ws, err := websocket.NewClient(cfg, conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("htunnel-ws: handshake fail: %w", err)
	}
	ws.MaxPayloadBytes = 32 << 20
	markHTCarrierResult(proxy.Name, true)
	util.LogModuleInfo("htunnel", "[HT-WS] bind websocket established: proxy=%s dst=%s port=%d", proxy.Name, dstAddr, bindPort)
	return &wsTargetConn{Conn: ws, crypto: crypto}, nil
}

// dialP2PWebSocket opens a persistent WebSocket to the h_tunnel server and
// wraps it as a frame transport. The server allocates the P2P session and
// speaks the same length-prefixed frame batches in both directions.
func dialP2PWebSocket(proxy *config.Proxy, crypto *util.HTunnelCrypto) (frame.FrameTransport, error) {
	u, origin, err := wsURLFor(proxy)
	if err != nil {
		return nil, err
	}
	cfg, err := websocket.NewConfig(u.String(), origin)
	if err != nil {
		return nil, fmt.Errorf("htunnel-ws: config: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialWSConn(ctx, proxy, u)
	if err != nil {
		return nil, fmt.Errorf("htunnel-ws: dial fail: %w", err)
	}
	ws, err := websocket.NewClient(cfg, conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("htunnel-ws: handshake fail: %w", err)
	}
	ws.MaxPayloadBytes = 32 << 20
	markHTCarrierResult(proxy.Name, true)
	util.LogModuleError("htunnel", "[HT-WS] persistent websocket established: proxy=%s url=%s", proxy.Name, u.String())
	return &frame.WSTransport{
		Conn: ws,
		Seal: crypto.SealBody,
		Open: crypto.OpenBody,
	}, nil
}
