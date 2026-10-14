//go:build windows

package tun

import (
	"net"

	"phaethon/util"
)

// setSystemDNS redirects the TUN interface DNS to the TUN DNS hijacker address
// and lowers the interface metric so Windows actually prefers it for resolution.
// It also flushes the Windows DNS resolver cache so that entries cached before
// TUN started (real IPs) are discarded and fresh queries go through the TUN
// DNS hijacker (returning Fake-IPs for domain-based rule matching).
func setSystemDNS(tunName, tunIP string) error {
	luid, index, err := getInterfaceLUID(tunName)
	if err != nil {
		return err
	}
	if err := setInterfaceDNSAPI(luid, index, []net.IP{net.ParseIP(tunIP).To4()}); err != nil {
		return err
	}
	if err := setInterfaceMetricAPI(tunName, 5, false); err != nil {
		util.LogModuleWarn("dns-setup", "set interface metric for %s fail: %v", tunName, err)
	}
	util.LogModuleInfo("dns-setup", "system dns for %s set to %s", tunName, tunIP)

	if err := flushDNSResolverCacheAPI(); err != nil {
		util.LogModuleWarn("dns-setup", "flush dns cache fail: %v", err)
	} else {
		util.LogModuleInfo("dns-setup", "dns cache flushed")
	}
	return nil
}

// restoreSystemDNS restores the TUN interface DNS to DHCP and resets metric.
func restoreSystemDNS(tunName string) {
	luid, index, err := getInterfaceLUID(tunName)
	if err != nil {
		util.LogModuleWarn("dns-setup", "restore dns for %s fail: %v", tunName, err)
		return
	}
	if err := clearInterfaceDNSAPI(luid, index); err != nil {
		util.LogModuleWarn("dns-setup", "restore dns for %s fail: %v", tunName, err)
	} else {
		util.LogModuleInfo("dns-setup", "system dns for %s restored to dhcp", tunName)
	}
	if err := setInterfaceMetricAPI(tunName, 0, true); err != nil {
		util.LogModuleWarn("dns-setup", "restore interface metric for %s fail: %v", tunName, err)
	}
}
