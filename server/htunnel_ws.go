package server

import (
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/net/websocket"

	"phaethon/db"
	"phaethon/dialer"
	"phaethon/frame"
	"phaethon/reverse"
	"phaethon/util"
)

// handleMeshWebSocket upgrades an h_tunnel HTTP request to a persistent
// WebSocket. Three modes share the same carrier, dispatched by headers:
//   - bind (X-C: BIND): reverse data / control channel (PORT 0/1/2);
//   - target (X-H/X-P present): a normal proxy connection spliced to the target
//     socket, used when the proxy itself is the WebSocket carrier;
//   - mesh (no target headers): one P2P session, length-prefixed frame batches.
func (s *HTunnelServer) handleMeshWebSocket(w http.ResponseWriter, r *http.Request) {
	crypto := util.NewHTunnelCrypto(s.Password)
	if r.Header.Get(dialer.HeaderCommand) == "BIND" {
		s.handleBindWebSocket(w, r, crypto)
		return
	}
	if r.Header.Get(dialer.HeaderTargetHost) != "" {
		s.handleTargetWebSocket(w, r, crypto)
		return
	}
	websocket.Handler(func(ws *websocket.Conn) {
		tr := &frame.WSTransport{
			Conn: ws,
			Seal: crypto.SealBody,
			Open: crypto.OpenBody,
		}
		util.LogModuleError("htunnel", "[HT-WS] mesh websocket established: %s", r.RemoteAddr)
		// handleP2PTransport blocks for a primary session, but returns
		// immediately when the first frame is a StripeAttach (ownership of the
		// transport moves to the existing session's stripe layer). Returning
		// from this handler would close the socket, so keep it alive until the
		// transport owner closes it.
		go handleP2PTransport(tr, "mesh-ws:"+r.RemoteAddr)
		<-tr.Done()
	}).ServeHTTP(w, r)
}

// handleTargetWebSocket serves one normal (non-mesh) proxy connection over a
// WebSocket: the target comes from the same encrypted X-H/X-P headers the HTTP
// carrier uses, then bytes are spliced in both directions.
// handleBindWebSocket serves one BIND channel (reverse data / control) over a
// WebSocket. It mirrors handleConnectionRequest's reverse semantics: the same
// encrypted headers select the address and the bind port, and the handler runs
// against a byte-stream net.Conn backed by WS messages.
func (s *HTunnelServer) handleBindWebSocket(w http.ResponseWriter, r *http.Request, crypto *util.HTunnelCrypto) {
	address, err := crypto.OpenHeader(r.Header.Get(dialer.HeaderTargetHost))
	if err != nil || address == "" {
		http.Error(w, "", http.StatusBadRequest)
		return
	}
	portStr, err := crypto.OpenHeader(r.Header.Get(dialer.HeaderTargetPort))
	if err != nil {
		http.Error(w, "", http.StatusBadRequest)
		return
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > reverse.BindPortP2P {
		http.Error(w, "", http.StatusBadRequest)
		return
	}
	if port == reverse.BindPortData && !db.HasReverseAddress(address) {
		http.Error(w, "", http.StatusForbidden)
		return
	}

	websocket.Handler(func(ws *websocket.Conn) {
		ws.MaxPayloadBytes = 32 << 20
		conn := &wsServerConn{Conn: ws, crypto: crypto}
		defer conn.Close()
		util.LogModuleInfo("htunnel", "[HT-WS] bind websocket %s: dst=%s port=%d", r.RemoteAddr, address, port)

		done := make(chan struct{})
		go func() {
			defer close(done)
			switch port {
			case reverse.BindPortControl:
				handleControlConnection(conn, address)
			case reverse.BindPortP2P:
				handleP2PConnection(conn, address)
			default:
				reverse.HandleReverseConnection(conn, address)
			}
		}()
		<-done
	}).ServeHTTP(w, r)
}

// wsServerConn adapts the message-oriented WebSocket to a net.Conn byte stream
// on the server side (the mirror of dialer.wsTargetConn).
type wsServerConn struct {
	*websocket.Conn
	crypto *util.HTunnelCrypto

	mu       sync.Mutex
	buf      []byte
	closeOne sync.Once
}

func (c *wsServerConn) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := websocket.Message.Send(c.Conn, c.crypto.SealBody(p)); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *wsServerConn) Read(p []byte) (int, error) {
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

func (c *wsServerConn) Close() error {
	c.closeOne.Do(func() { _ = c.Conn.Close() })
	return nil
}

func (c *wsServerConn) LocalAddr() net.Addr  { return &net.TCPAddr{} }
func (c *wsServerConn) RemoteAddr() net.Addr { return &net.TCPAddr{} }

func (c *wsServerConn) SetDeadline(time.Time) error      { return nil }
func (c *wsServerConn) SetReadDeadline(time.Time) error  { return nil }
func (c *wsServerConn) SetWriteDeadline(time.Time) error { return nil }

// handleTargetWebSocket serves one normal (non-mesh) proxy connection over a
// WebSocket: the target comes from the same encrypted X-H/X-P headers the HTTP
// carrier uses, then bytes are spliced in both directions.
func (s *HTunnelServer) handleTargetWebSocket(w http.ResponseWriter, r *http.Request, crypto *util.HTunnelCrypto) {
	host, err := crypto.OpenHeader(r.Header.Get(dialer.HeaderTargetHost))
	if err != nil || host == "" {
		http.Error(w, "", http.StatusBadRequest)
		return
	}
	portStr, err := crypto.OpenHeader(r.Header.Get(dialer.HeaderTargetPort))
	if err != nil {
		http.Error(w, "", http.StatusBadRequest)
		return
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		http.Error(w, "", http.StatusBadRequest)
		return
	}
	websocket.Handler(func(ws *websocket.Conn) {
		target, derr := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 10*time.Second)
		if derr != nil {
			util.LogModuleWarn("htunnel", "[HT-WS] target dial fail %s:%d: %v", host, port, derr)
			return
		}
		defer target.Close()
		util.LogModuleInfo("htunnel", "[HT-WS] target websocket %s -> %s:%d", r.RemoteAddr, host, port)

		go func() {
			for {
				var msg []byte
				if err := websocket.Message.Receive(ws, &msg); err != nil {
					return
				}
				if len(msg) == 0 {
					continue
				}
				plain, oerr := crypto.OpenBody(msg)
				if oerr != nil {
					return
				}
				if _, werr := target.Write(plain); werr != nil {
					return
				}
			}
		}()

		buf := make([]byte, 32<<10)
		for {
			n, rerr := target.Read(buf)
			if n > 0 {
				if serr := websocket.Message.Send(ws, crypto.SealBody(buf[:n])); serr != nil {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}).ServeHTTP(w, r)
}
