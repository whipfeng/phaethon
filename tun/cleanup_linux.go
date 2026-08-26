//go:build linux

package tun

import (
	"net"
	"os/exec"
	"strings"

	"github.com/vishvananda/netlink"
	"phaethon/util"
)

// CleanupResidual removes leftover routes and brings down any TUN interface
// that still holds the TUN IP address.
func CleanupResidual() {
	// 1. Delete split-tunnel routes (current scheme)
	if link, err := netlink.LinkByName("tun0"); err == nil {
		for _, cidr := range []string{"0.0.0.0/1", "128.0.0.0/1"} {
			_, dst, _ := net.ParseCIDR(cidr)
			rt := &netlink.Route{LinkIndex: link.Attrs().Index, Dst: dst}
			_ = netlink.RouteDel(rt)
		}
		// Delete old-style default route (backward compat)
		rt := &netlink.Route{LinkIndex: link.Attrs().Index, Dst: nil}
		_ = netlink.RouteDel(rt)
	}

	// 2. Find and down any TUN interface holding the TUN address.
	if ifaces, err := net.Interfaces(); err == nil {
		for _, iface := range ifaces {
			if !strings.HasPrefix(iface.Name, "tun") {
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
					if out, err := exec.Command("ip", "link", "set", iface.Name, "down").CombinedOutput(); err != nil {
						util.LogWarn("tun: cleanup ip link set %s down fail: %v, %s", iface.Name, err, out)
					} else {
						util.LogInfo("tun: cleanup brought %s down", iface.Name)
					}
					break
				}
			}
		}
	}
}

// InterfaceExists reports whether any TUN interface is currently present.
func InterfaceExists() bool {
	ifaces, err := net.Interfaces()
	if err != nil {
		return false
	}
	for _, iface := range ifaces {
		if strings.HasPrefix(iface.Name, "tun") {
			return true
		}
	}
	return false
}
