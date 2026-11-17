//go:build darwin

package tun

import (
	"net"
	"strings"
)

// CleanupResidual restores persistent host state left behind by a previous
// abnormal exit: DNS and preempted third-party split routes.
//
// It does not touch utun interfaces. An utun lives and dies with its fd; if one
// still exists at startup it belongs to a live process or another application,
// so deleting/clearing it by name or address would be a guess.
func CleanupResidual() {
	restoreDarwinDNSBackup()
	restorePersistedForeignSplits()
}

// InterfaceExists reports whether any utun interface is currently present.
func InterfaceExists() bool {
	ifaces, err := net.Interfaces()
	if err != nil {
		return false
	}
	for _, iface := range ifaces {
		if strings.HasPrefix(iface.Name, "utun") {
			return true
		}
	}
	return false
}
