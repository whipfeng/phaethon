package tun

import (
	"net"
	"phaethon/config"
)

// DHCPServer provides DHCP service on the physical LAN interface.
// Only active when bypass-gateway is enabled.
type DHCPServer interface {
	Start() error
	Stop()
	ActiveLeases() int
}

// newDHCPServer creates a DHCP server bound to the given interface.
// dataDir is used for persistent lease storage. Returns (nil, nil) on non-Linux platforms.
func newDHCPServer(ifaceName string, cfg *config.DHCPConfig, dnsAddr net.IP, dataDir string) (DHCPServer, error) {
	return newDHCPServerImpl(ifaceName, cfg, dnsAddr, dataDir)
}
