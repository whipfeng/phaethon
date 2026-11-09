package server

import (
	"fmt"
	"net"

	"phaethon/config"
	"phaethon/util"
)

// StartListener starts the inbound listener for a mapping by its type.
func StartListener(m *config.Mapping) (net.Listener, error) {
	if m.Type == "socks5" && m.Password == "" {
		util.LogModuleWarn("server",
			"mapping %s: socks5 has no password; it runs as an unauthenticated open proxy on %s:%d",
			m.Name, normalizeListenAddr(m.ListenAddr), m.Port)
	}
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
