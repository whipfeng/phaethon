//go:build windows

package tun

import (
	"fmt"
	"os/exec"

	"phaethon/util"
)

// setSystemDNS redirects the TUN interface DNS to the given TUN IP.
func setSystemDNS(devName, tunIP string) error {
	cmd := exec.Command("netsh", "interface", "ip", "set", "dns", "name="+devName, "static", tunIP)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("set dns: %v: %s", err, out)
	}
	util.LogInfo("tun: system dns for %s set to %s", devName, tunIP)
	return nil
}

// restoreSystemDNS restores the TUN interface DNS to DHCP.
func restoreSystemDNS(devName string) {
	if out, err := exec.Command("netsh", "interface", "ip", "set", "dns", "name="+devName, "dhcp").CombinedOutput(); err != nil {
		util.LogWarn("tun: restore dns for %s fail: %v, %s", devName, err, out)
	} else {
		util.LogInfo("tun: system dns for %s restored to dhcp", devName)
	}
}
