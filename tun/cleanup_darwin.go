//go:build darwin

package tun

import (
	"net"
	"os/exec"
	"strings"

	"phaethon/util"
)

// CleanupResidual removes leftover routes and clears any utun interface
// that still holds the TUN address (or a legacy 198.18.x.x address).
// tunIP is the expected TUN interface address (mesh subnet+2); nil skips
// the address match and only legacy/route cleanup runs.
func CleanupResidual(tunIP net.IP) {
	// 1. Delete split-tunnel routes.
	exec.Command("route", "-n", "delete", "-net", "0.0.0.0/1").Run()
	exec.Command("route", "-n", "delete", "-net", "128.0.0.0/1").Run()

	// 2. Find and down any utun interface holding the TUN address.
	if ifaces, err := net.Interfaces(); err == nil {
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
				if ip4 == nil {
					continue
				}
				match := ip4[0] == 198 && ip4[1] >= 18 && ip4[1] <= 19
				if !match && tunIP != nil {
					match = ip4.Equal(tunIP.To4())
				}
				if !match {
					continue
				}
				// Delete the address first: split-tunnel routes pinned to the
				// interface are removed by the kernel along with the address.
				if out, err := exec.Command("ifconfig", iface.Name, "delete", ip4.String()).CombinedOutput(); err != nil {
					util.LogModuleWarn("cleanup", "cleanup ifconfig %s delete %s fail: %v, %s", iface.Name, ip4, err, out)
				}
				if out, err := exec.Command("ifconfig", iface.Name, "down").CombinedOutput(); err != nil {
					util.LogModuleWarn("cleanup", "cleanup ifconfig %s down fail: %v", iface.Name, err, out)
				} else {
					util.LogModuleInfo("cleanup", "cleanup cleared %s (%s)", iface.Name, ip4)
				}
				break
			}
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
