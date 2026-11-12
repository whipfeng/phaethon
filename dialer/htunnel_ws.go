package dialer

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
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
	util.LogModuleError("htunnel", "[HT-WS] persistent websocket established: proxy=%s url=%s", proxy.Name, u.String())
	return &frame.WSTransport{
		Conn: ws,
		Seal: crypto.SealBody,
		Open: crypto.OpenBody,
	}, nil
}
