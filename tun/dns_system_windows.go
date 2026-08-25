//go:build windows

package tun

import (
	"fmt"
	"os/exec"

	"phaethon/util"
)

// setSystemDNS redirects the physical interface DNS to the given TUN IP.
func setSystemDNS(ifaceName, tunIP string) error {
	cmd := exec.Command("netsh", "interface", "ip", "set", "dns", "name="+ifaceName, "static", tunIP)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("set dns: %v: %s", err, out)
	}
	util.LogInfo("tun: system dns for %s set to %s", ifaceName, tunIP)
	return nil
}

// restoreSystemDNS restores the physical interface DNS to DHCP.
func restoreSystemDNS(ifaceName string) {
	if out, err := exec.Command("netsh", "interface", "ip", "set", "dns", "name="+ifaceName, "dhcp").CombinedOutput(); err != nil {
		util.LogWarn("tun: restore dns for %s fail: %v, %s", ifaceName, err, out)
	} else {
		util.LogInfo("tun: system dns for %s restored to dhcp", ifaceName)
	}
}
