//go:build darwin

package tun

import (
	"net"
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

// InterfaceExists reports whether the named utun interface is currently present.
func InterfaceExists(name string) bool {
	if name == "" {
		return false
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return false
	}
	for _, iface := range ifaces {
		if iface.Name == name {
			return true
		}
	}
	return false
}
