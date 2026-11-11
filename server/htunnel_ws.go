package server

import (
	"net/http"

	"golang.org/x/net/websocket"

	"phaethon/frame"
	"phaethon/util"
)

// handleMeshWebSocket upgrades an h_tunnel HTTP request to a persistent
// WebSocket and runs one P2P session directly on it. Each WS binary message
// carries length-prefixed frames in both directions, so data and ACKs share a
// single full-duplex connection instead of GET long-polls plus POSTs.
func (s *HTunnelServer) handleMeshWebSocket(w http.ResponseWriter, r *http.Request) {
	websocket.Handler(func(ws *websocket.Conn) {
		crypto := util.NewHTunnelCrypto(s.Password)
		tr := &frame.WSTransport{
			Conn: ws,
			Seal: crypto.SealBody,
			Open: crypto.OpenBody,
		}
		util.LogModuleError("htunnel", "[HT-WS] mesh websocket established: %s", r.RemoteAddr)
		handleP2PTransport(tr, "mesh-ws:"+r.RemoteAddr)
	}).ServeHTTP(w, r)
}
