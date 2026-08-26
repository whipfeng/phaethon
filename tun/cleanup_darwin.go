//go:build darwin

package tun

import (
	"net"
	"os/exec"
	"strings"

	"phaethon/util"
)

// CleanupResidual removes leftover routes and brings down any utun interface
// that still holds the TUN IP address.
func CleanupResidual() {
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
				if ip4[0] == 198 && ip4[1] >= 18 && ip4[1] <= 19 {
					if out, err := exec.Command("ifconfig", iface.Name, "down").CombinedOutput(); err != nil {
						util.LogWarn("tun: cleanup ifconfig %s down fail: %v, %s", iface.Name, err, out)
					} else {
						util.LogInfo("tun: cleanup brought %s down", iface.Name)
					}
					break
				}
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
