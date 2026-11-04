//go:build darwin

package tun

import (
	"net"
	"os/exec"
	"strings"

	"phaethon/util"
)

// CleanupResidual removes leftover TUN interface state from a previous crash.
// Only the interface holding exactly tunIP is touched: the address is removed
// (darwin drops the interface's split-tunnel routes along with the address),
// and the interface itself is left alone. Routes and interfaces not owned by
// phaethon are never modified. tunIP is the expected TUN address (mesh
// subnet+2); nil skips the interface scan entirely.
func CleanupResidual(tunIP net.IP) {
	target := tunIP.To4()
	if target == nil {
		util.LogModuleInfo("cleanup", "skip interface scan: no tunIP")
		return
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		util.LogModuleWarn("cleanup", "list interfaces fail: %v", err)
		return
	}
	for _, iface := range ifaces {
		if !strings.HasPrefix(iface.Name, "utun") {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipNet.IP.To4()
			if ip4 == nil || !ip4.Equal(target) {
				continue
			}
			if out, err := exec.Command("ifconfig", iface.Name, "delete", ip4.String()).CombinedOutput(); err != nil {
				util.LogModuleWarn("cleanup", "cleanup ifconfig %s delete %s fail: %v, %s", iface.Name, ip4, err, out)
			} else {
				util.LogModuleInfo("cleanup", "cleanup cleared %s (%s)", iface.Name, ip4)
			}
			break
		}
	}
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
