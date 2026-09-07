//go:build linux

package tun

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"strings"

	"github.com/vishvananda/netlink"
	"phaethon/util"
)

// CleanupResidual removes leftover routes, restores system DNS, kernel parameters,
// and brings down any TUN interface that still holds the TUN IP address.
// This is called by the watchdog when the main process crashes.
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

	// 2. Restore resolv.conf from backup if it exists.
	// When TUN is active, resolv.conf points to 192.0.2.3 (DNSHijacker).
	// If the main process crashes, DNS becomes unreachable.
	const resolvConf = "/etc/resolv.conf"
	const backupPath = resolvConf + ".phaethon.bak"
	if _, err := os.Stat(backupPath); err == nil {
		if origData, err := os.ReadFile(backupPath); err == nil {
			if err := os.WriteFile(resolvConf, origData, 0644); err == nil {
				util.LogInfo("tun: cleanup restored resolv.conf")
			} else {
				util.LogWarn("tun: cleanup restore resolv.conf fail: %v", err)
			}
		} else {
			util.LogWarn("tun: cleanup read resolv.conf backup fail: %v", err)
		}
		_ = os.Remove(backupPath)
	}

	// 3. Read state file and restore kernel parameters (rp_filter, ip_forward).
	// The main process saves these values before modifying them.
	if data, err := os.ReadFile(tunStateFile); err == nil {
		var state tunState
		if json.Unmarshal(data, &state) == nil {
			// Restore rp_filter on the physical interface
			if state.IfaceName != "" {
				if err := writeRpFilter(state.IfaceName, state.RpFilter); err == nil {
					util.LogInfo("tun: cleanup restored %s rp_filter=%d", state.IfaceName, state.RpFilter)
				} else {
					util.LogWarn("tun: cleanup restore rp_filter fail: %v", err)
				}
				// Also restore "all" to the common relaxed value
				_ = writeRpFilter("all", 2)
			}
			// Restore ip_forward
			if err := writeIPForward(state.IPForward); err == nil {
				util.LogInfo("tun: cleanup restored ip_forward=%d", state.IPForward)
			} else {
				util.LogWarn("tun: cleanup restore ip_forward fail: %v", err)
			}
		} else {
			util.LogWarn("tun: cleanup parse state file fail: %v", err)
		}
		_ = os.Remove(tunStateFile)
	}

	// 4. Find and down any TUN interface holding the TUN address.
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
