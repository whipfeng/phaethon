package server

import (
	"net"

	"phaethon/p2p"
)

// handleP2PConnection handles an incoming P2P connection (PORT=2).
// The connection is already past the protocol handshake (SOCKS5/Trojan/HTunnel).
func handleP2PConnection(conn net.Conn, address string) {
	p2p.HandleP2PConnection(conn, address)
}
