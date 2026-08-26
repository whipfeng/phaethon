//go:build windows

package tun

import (
	"fmt"
	"os/exec"

	"phaethon/util"
)

// setSystemDNS redirects the TUN interface DNS to the TUN DNS hijacker address
// and lowers the interface metric so Windows actually prefers it for resolution.
func setSystemDNS(tunName, tunIP string) error {
	if out, err := exec.Command("netsh", "interface", "ip", "set", "dns", "name="+tunName, "static", tunIP).CombinedOutput(); err != nil {
		return fmt.Errorf("set dns: %v: %s", err, out)
	}
	if out, err := exec.Command("netsh", "interface", "ipv4", "set", "interface", "name="+tunName, "metric=5").CombinedOutput(); err != nil {
		util.LogWarn("tun: set interface metric for %s fail: %v, %s", tunName, err, out)
	}
	util.LogInfo("tun: system dns for %s set to %s", tunName, tunIP)
	return nil
}

// restoreSystemDNS restores the TUN interface DNS to DHCP and resets metric.
func restoreSystemDNS(tunName string) {
	if out, err := exec.Command("netsh", "interface", "ip", "set", "dns", "name="+tunName, "dhcp").CombinedOutput(); err != nil {
		util.LogWarn("tun: restore dns for %s fail: %v, %s", tunName, err, out)
	} else {
		util.LogInfo("tun: system dns for %s restored to dhcp", tunName)
	}
	if out, err := exec.Command("netsh", "interface", "ipv4", "set", "interface", "name="+tunName, "metric=automatic").CombinedOutput(); err != nil {
		util.LogWarn("tun: restore interface metric for %s fail: %v, %s", tunName, err, out)
	}
}
