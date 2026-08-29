//go:build windows

package tun

import (
	"fmt"
	"net"
	"os/exec"

	"phaethon/util"
)

// setSystemDNS redirects the TUN interface DNS to the TUN DNS hijacker address
// and lowers the interface metric so Windows actually prefers it for resolution.
func setSystemDNS(tunName, tunIP string) error {
	luid, index, err := getInterfaceLUID(tunName)
	if err != nil {
		return err
	}
	if err := setInterfaceDNSAPI(luid, index, []net.IP{net.ParseIP(tunIP).To4()}); err != nil {
		// Fallback to netsh if the API fails (e.g., ERROR_INVALID_PARAMETER on some Windows versions)
		util.LogWarn("tun: setInterfaceDNSAPI failed, falling back to netsh: %v", err)
		if err := setDNSViaNetsh(tunName, tunIP); err != nil {
			return err
		}
	}
	if err := setInterfaceMetricAPI(tunName, 5, false); err != nil {
		util.LogWarn("tun: set interface metric for %s fail: %v", tunName, err)
	}
	util.LogInfo("tun: system dns for %s set to %s", tunName, tunIP)
	return nil
}

// setDNSViaNetsh sets DNS using netsh as a fallback when the native API fails.
func setDNSViaNetsh(ifaceName, dnsServer string) error {
	cmd := exec.Command("netsh", "interface", "ip", "set", "dns",
		"name="+ifaceName, "static", dnsServer, "primary")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("netsh set dns failed: %v, output: %s", err, string(out))
	}
	return nil
}

// restoreSystemDNS restores the TUN interface DNS to DHCP and resets metric.
func restoreSystemDNS(tunName string) {
	luid, index, err := getInterfaceLUID(tunName)
	if err != nil {
		util.LogWarn("tun: restore dns for %s fail: %v", tunName, err)
		return
	}
	if err := clearInterfaceDNSAPI(luid, index); err != nil {
		// Fallback to netsh if the API fails
		util.LogWarn("tun: clearInterfaceDNSAPI failed, falling back to netsh: %v", err)
		_ = clearDNSViaNetsh(tunName)
	} else {
		util.LogInfo("tun: system dns for %s restored to dhcp", tunName)
	}
	if err := setInterfaceMetricAPI(tunName, 0, true); err != nil {
		util.LogWarn("tun: restore interface metric for %s fail: %v", tunName, err)
	}
}

// clearDNSViaNetsh clears DNS using netsh as a fallback.
func clearDNSViaNetsh(ifaceName string) error {
	cmd := exec.Command("netsh", "interface", "ip", "set", "dns",
		"name="+ifaceName, "source=dhcp")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("netsh clear dns failed: %v, output: %s", err, string(out))
	}
	return nil
}
