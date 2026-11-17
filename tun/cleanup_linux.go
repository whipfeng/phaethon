//go:build linux

package tun

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"strings"

	"phaethon/util"
)

// CleanupResidual restores persistent host state left behind by a previous
// abnormal exit: DNS/resolv.conf, rp_filter/ip_forward and the phaethon-owned
// iptables FORWARD tuples.
//
// It deliberately does NOT touch any network interface. A Linux TUN interface
// created with TUNSETIFF (without IFF_PERSIST) lives and dies with its fd:
// process exit, including SIGKILL, destroys tun0 in the kernel. If a tun0
// still exists at startup it belongs to a live process or was created
// externally (e.g. `ip tuntap add`), so deleting it by name would be a guess.
// In that case StartTUN fails with a clear diagnostic and the node stays in
// visible degraded mesh-only mode until the operator resolves the conflict.
func CleanupResidual() {
	// Restore resolved/nmcli DNS recorded before TUN started (D20). The
	// resolv.conf path has its own .phaethon.bak recovery below.
	restorePersistedLinuxDNS()

	// 1. Restore resolv.conf from backup if it exists.
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

	// 2. Read state file and restore kernel parameters (rp_filter, ip_forward).
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
				for {
					if err := exec.Command("iptables", "-D", "FORWARD", "-i", state.IfaceName, "-o", "tun0", "-j", "ACCEPT").Run(); err != nil {
						break
					}
					util.LogModuleInfo("cleanup", "cleanup removed iptables FORWARD %s->tun0", state.IfaceName)
				}
				for {
					if err := exec.Command("iptables", "-D", "FORWARD", "-i", "tun0", "-o", state.IfaceName, "-j", "ACCEPT").Run(); err != nil {
						break
					}
					util.LogModuleInfo("cleanup", "cleanup removed iptables FORWARD tun0->%s", state.IfaceName)
				}
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
