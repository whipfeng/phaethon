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

// CleanupResidual removes leftover TUN interface state and restores system
// configuration from a previous crash. Interface/route cleanup only touches
// the tun* interface holding exactly tunIP; the address is removed (the
// link-scoped split-tunnel routes are deleted first) and the interface itself
// is left up. resolv.conf / kernel-parameter / iptables restore runs
// unconditionally — those are phaethon-owned artifacts independent of the
// interface match. tunIP is the expected TUN address (mesh subnet+2); nil
// skips the interface scan but still restores system state.
func CleanupResidual(tunIP net.IP) {
	// Restore resolved/nmcli DNS recorded before TUN started (D20). The
	// resolv.conf path has its own .phaethon.bak recovery below.
	restorePersistedLinuxDNS()

	// 1. Find the tun* interface holding tunIP and clear only that one.
	target := tunIP.To4()
	if target != nil {
		if link := findTUNLinkByIP(target); link != nil {
			for _, cidr := range []string{"0.0.0.0/1", "128.0.0.0/1"} {
				_, dst, _ := net.ParseCIDR(cidr)
				_ = netlink.RouteDel(&netlink.Route{LinkIndex: link.Attrs().Index, Dst: dst})
			}
			for _, addr := range linkAddrs4(link) {
				if addr.IP.Equal(target) {
					if err := netlink.AddrDel(link, &addr); err != nil {
						util.LogModuleWarn("cleanup", "cleanup addr del %s dev %s fail: %v", addr.IP, link.Attrs().Name, err)
					} else {
						util.LogModuleInfo("cleanup", "cleanup cleared %s (%s)", link.Attrs().Name, addr.IP)
					}
					break
				}
			}
		}
	} else {
		util.LogModuleInfo("cleanup", "skip interface scan: no tunIP")
	}

	// 2. Restore resolv.conf from backup if it exists.
	// When TUN is active, resolv.conf points to the mesh-derived DNS address
	// (e.g., 100.0.0.3 for subnet 100.0.0.0/16, which is the DNSHijacker).
	// If the main process crashes, DNS becomes unreachable.
	const resolvConf = "/etc/resolv.conf"
	const backupPath = resolvConf + ".phaethon.bak"
	if _, err := os.Stat(backupPath); err == nil {
		if origData, err := os.ReadFile(backupPath); err == nil {
			if err := os.WriteFile(resolvConf, origData, 0644); err == nil {
				util.LogModuleInfo("cleanup", "cleanup restored resolv.conf")
			} else {
				util.LogModuleWarn("cleanup", "cleanup restore resolv.conf fail: %v", err)
			}
		} else {
			util.LogModuleWarn("cleanup", "cleanup read resolv.conf backup fail: %v", err)
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
					util.LogModuleInfo("cleanup", "cleanup restored %s rp_filter=%d", state.IfaceName, state.RpFilter)
				} else {
					util.LogModuleWarn("cleanup", "cleanup restore rp_filter fail: %v", err)
				}
				// Also restore "all" to the common relaxed value
				_ = writeRpFilter("all", 2)
			}
			// Restore ip_forward
			if err := writeIPForward(state.IPForward); err == nil {
				util.LogModuleInfo("cleanup", "cleanup restored ip_forward=%d", state.IPForward)
			} else {
				util.LogModuleWarn("cleanup", "cleanup restore ip_forward fail: %v", err)
			}
			// Clean up iptables FORWARD rules - loop to remove ALL duplicates
			if state.IfaceName != "" {
				// Remove all eth1->tun0 rules
				for {
					if err := exec.Command("iptables", "-D", "FORWARD", "-i", state.IfaceName, "-o", "tun0", "-j", "ACCEPT").Run(); err != nil {
						break
					}
					util.LogModuleInfo("cleanup", "cleanup removed iptables FORWARD %s->tun0", state.IfaceName)
				}
				// Remove all tun0->eth1 rules
				for {
					if err := exec.Command("iptables", "-D", "FORWARD", "-i", "tun0", "-o", state.IfaceName, "-j", "ACCEPT").Run(); err != nil {
						break
					}
					util.LogModuleInfo("cleanup", "cleanup removed iptables FORWARD tun0->%s", state.IfaceName)
				}
				// Remove all eth1->eth1 rules
				for {
					if err := exec.Command("iptables", "-D", "FORWARD", "-i", state.IfaceName, "-o", state.IfaceName, "-j", "ACCEPT").Run(); err != nil {
						break
					}
					util.LogModuleInfo("cleanup", "cleanup removed iptables FORWARD %s->%s", state.IfaceName, state.IfaceName)
				}
			}
		} else {
			util.LogModuleWarn("cleanup", "cleanup parse state file fail: %v", err)
		}
		_ = os.Remove(tunStateFile)
	}
}

// findTUNLinkByIP returns the first tun* interface holding exactly ip, or nil.
func findTUNLinkByIP(ip net.IP) netlink.Link {
	links, err := netlink.LinkList()
	if err != nil {
		util.LogModuleWarn("cleanup", "list links fail: %v", err)
		return nil
	}
	for _, link := range links {
		if !strings.HasPrefix(link.Attrs().Name, "tun") {
			continue
		}
		for _, addr := range linkAddrs4(link) {
			if addr.IP.Equal(ip) {
				return link
			}
		}
	}
	return nil
}

// linkAddrs4 lists IPv4 addresses on a link, tolerating per-link errors.
func linkAddrs4(link netlink.Link) []netlink.Addr {
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return nil
	}
	return addrs
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
