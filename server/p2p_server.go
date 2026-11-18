package server

import (
	"net"

	"phaethon/frame"
	"phaethon/p2p"
)

// handleP2PConnection handles an incoming P2P connection (PORT=2).
// The connection is already past the protocol handshake (SOCKS5/Trojan/HTunnel).
// legSecret is the accepting proxy's own credential; it keys the link's UDP
// leg and must match the dialer's contract on the active side.
func handleP2PConnection(conn net.Conn, address, legSecret string) {
	p2p.HandleP2PConnection(conn, address, legSecret)
}

// handleP2PTransport runs a P2P session over an established frame transport
// (htunnel mesh channel path — no reverse server splice).
func handleP2PTransport(t frame.FrameTransport, address, legSecret string) {
	p2p.HandleP2PTransport(t, address, legSecret)
}
