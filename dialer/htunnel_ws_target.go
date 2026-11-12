package dialer

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"

	"golang.org/x/net/websocket"

	"phaethon/config"
	"phaethon/util"
)

// dialWSTarget opens a WebSocket for a normal (non-mesh) proxy target and
// returns a net.Conn carrying the target byte stream. The target travels in
// the same encrypted X-H/X-P headers as the HTTP carrier; the server dials the
// target and splices the WS messages to that socket.
func dialWSTarget(ctx context.Context, proxy *config.Proxy, crypto *util.HTunnelCrypto, host string, port int) (net.Conn, error) {
	u, origin, err := wsURLFor(proxy)
	if err != nil {
		return nil, err
	}
	cfg, err := websocket.NewConfig(u.String(), origin)
	if err != nil {
		return nil, fmt.Errorf("htunnel-ws: config: %w", err)
	}
	cfg.Header.Set(HeaderTargetHost, crypto.SealHeader(host))
	cfg.Header.Set(HeaderTargetPort, crypto.SealHeader(strconv.Itoa(port)))

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
	util.LogModuleInfo("htunnel", "[HT-WS] target websocket established: proxy=%s target=%s:%d", proxy.Name, host, port)
	return &wsTargetConn{Conn: ws, crypto: crypto}, nil
}

// wsTargetConn adapts the message-oriented WebSocket to a net.Conn byte stream.
// Each Write is sealed into one message; each Read buffers one opened message.
type wsTargetConn struct {
	*websocket.Conn
	crypto *util.HTunnelCrypto

	mu  sync.Mutex
	buf []byte
}

func (c *wsTargetConn) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	data := c.crypto.SealBody(p)
	if err := websocket.Message.Send(c.Conn, data); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *wsTargetConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.buf) > 0 {
		n := copy(p, c.buf)
		c.buf = c.buf[n:]
		return n, nil
	}
	var msg []byte
	if err := websocket.Message.Receive(c.Conn, &msg); err != nil {
		return 0, err
	}
	plain, err := c.crypto.OpenBody(msg)
	if err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	n := copy(p, plain)
	if n < len(plain) {
		c.buf = append([]byte(nil), plain[n:]...)
	}
	return n, nil
}
