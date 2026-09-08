package tun

import (
	"net"
	"phaethon/config"
	"time"
)

// DHCPLease represents a single DHCP lease for API/UI display.
type DHCPLease struct {
	IP       string    `json:"ip"`
	MAC      string    `json:"mac"`
	Hostname string    `json:"hostname,omitempty"`
	Expires  time.Time `json:"expires"`
}

// DHCPServer provides DHCP service on the physical LAN interface.
// Only active when bypass-gateway is enabled.
type DHCPServer interface {
	Start() error
	Stop()
	ActiveLeases() int
	Leases() []DHCPLease
	UpdateStaticBindings(bindings []config.DHCPStaticBinding)
}

// newDHCPServer creates a DHCP server bound to the given interface.
// dataDir is used for persistent lease storage. Returns (nil, nil) on non-Linux platforms.
func newDHCPServer(ifaceName string, cfg *config.DHCPConfig, dnsAddr net.IP, dataDir string) (DHCPServer, error) {
	return newDHCPServerImpl(ifaceName, cfg, dnsAddr, dataDir)
}
