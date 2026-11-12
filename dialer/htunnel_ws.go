package dialer

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/net/websocket"

	"phaethon/config"
	"phaethon/frame"
	"phaethon/util"
)

// wsEnabledFor reports whether the WebSocket carrier is enabled for a proxy.
// PHAETHON_HT_WS accepts "1"/"all" or a comma-separated proxy-name list
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

// dialP2PWebSocket opens a persistent WebSocket to the h_tunnel server and
// wraps it as a frame transport. The server allocates the P2P session and
// speaks the same length-prefixed frame batches in both directions.
func dialP2PWebSocket(proxy *config.Proxy, crypto *util.HTunnelCrypto) (frame.FrameTransport, error) {
	u, err := url.Parse(proxy.URL)
	if err != nil {
		return nil, fmt.Errorf("htunnel-ws: bad url: %w", err)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "ws", "wss":
	default:
		return nil, fmt.Errorf("htunnel-ws: unsupported scheme %q", u.Scheme)
	}
	origin := "http://" + u.Host
	if u.Scheme == "wss" {
		origin = "https://" + u.Host
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/ws/" + util.NextConnID()

	cfg, err := websocket.NewConfig(u.String(), origin)
	if err != nil {
		return nil, fmt.Errorf("htunnel-ws: config: %w", err)
	}
	// Bound the opening handshake: a proxy that blackholes Upgrade must not
	// stall the link dial; the caller falls back to GET/POST long-poll.
	cfg.Dialer = &net.Dialer{Timeout: 5 * time.Second}
	conn, err := websocket.DialConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("htunnel-ws: dial fail: %w", err)
	}
	conn.MaxPayloadBytes = 32 << 20
	util.LogModuleError("htunnel", "[HT-WS] persistent websocket established: proxy=%s url=%s", proxy.Name, u.String())
	return &frame.WSTransport{
		Conn: conn,
		Seal: crypto.SealBody,
		Open: crypto.OpenBody,
	}, nil
}
