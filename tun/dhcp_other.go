//go:build !linux

package tun

import (
	"net"
	"phaethon/config"
)

func newDHCPServerImpl(ifaceName string, cfg *config.DHCPConfig, dnsAddr net.IP, dataDir string) (DHCPServer, error) {
	return nil, nil
}
