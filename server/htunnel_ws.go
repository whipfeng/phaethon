package server

import (
	"net"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/net/websocket"

	"phaethon/dialer"
	"phaethon/frame"
	"phaethon/util"
)

// handleMeshWebSocket upgrades an h_tunnel HTTP request to a persistent
// WebSocket. It serves two modes behind the same carrier:
//   - mesh (no target headers): one P2P session, length-prefixed frame batches;
//   - target (X-H/X-P present): a normal proxy connection spliced to the target
//     socket, used when the proxy itself is the WebSocket carrier.
func (s *HTunnelServer) handleMeshWebSocket(w http.ResponseWriter, r *http.Request) {
	crypto := util.NewHTunnelCrypto(s.Password)
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
