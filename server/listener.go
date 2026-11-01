package server

import (
	"fmt"
	"net"

	"phaethon/config"
)

// StartListener starts the inbound listener for a mapping by its type.
func StartListener(m *config.Mapping) (net.Listener, error) {
	switch m.Type {
	case "socks5":
		return StartSocks5(m)
	case "direct", "DIRECT":
		return StartDirect(m)
	case "trojan":
		return StartTrojan(m)
	case "h_tunnel":
		return StartHTunnel(m)
	case "http":
		return StartHTTP(m)
	case "https":
		return StartHTTPS(m)
	default:
		return nil, fmt.Errorf("unsupported mapping type: %s", m.Type)
	}
}
