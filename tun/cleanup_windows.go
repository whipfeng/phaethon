//go:build windows

package tun

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
	"unsafe"

	"phaethon/util"
)

// CleanupResidual removes leftover TUN adapter and routes from a previous crash.
func CleanupResidual() {
	luid, index, err := getInterfaceLUID("phaethontun")
	if err == nil {
		// Delete split-tunnel routes (current scheme: 0/1 + 128/1)
		for _, prefix := range []struct {
			ip  net.IP
			len uint8
		}{
			{net.ParseIP("0.0.0.0").To4(), 1},
			{net.ParseIP("128.0.0.0").To4(), 1},
		} {
			var fwdRow mibIpForwardRow2
			fwdRow.init()
			fwdRow.setInterfaceLuid(luid)
			fwdRow.setInterfaceIndex(index)
			fwdRow.setDestinationPrefix(prefix.ip, prefix.len)
			fwdRow.setNextHop(net.IPv4zero)
			fwdRow.setMetric(1)
			procDeleteIpForwardEntry2.Call(uintptr(unsafe.Pointer(&fwdRow[0])))
		}

		// Delete old-style default route (0/0, backward compat)
		var fwdRow mibIpForwardRow2
		fwdRow.init()
		fwdRow.setInterfaceLuid(luid)
		fwdRow.setInterfaceIndex(index)
		fwdRow.setDestinationPrefix(net.IPv4zero, 0)
		fwdRow.setNextHop(net.IPv4zero)
		fwdRow.setMetric(1)
		procDeleteIpForwardEntry2.Call(uintptr(unsafe.Pointer(&fwdRow[0])))

		// Delete old-style default route with gateway=198.18.0.1 (backward compat)
		fwdRow.setNextHop(net.ParseIP("198.18.0.1"))
		procDeleteIpForwardEntry2.Call(uintptr(unsafe.Pointer(&fwdRow[0])))

		// Delete interface address
		ip := net.ParseIP("198.18.0.1").To4()
		var addrRow mibUnicastIpAddressRow
		addrRow.init()
		addrRow.setAddress(ip)
		addrRow.setInterfaceLuid(luid)
		addrRow.setInterfaceIndex(index)
		addrRow.setOnLinkPrefixLength(15)
		procDeleteUnicastIpAddressEntry.Call(uintptr(unsafe.Pointer(&addrRow[0])))
	}

	// Check if phaethontun adapter still exists and disable/remove it.
	out, _ := exec.Command("netsh", "interface", "show", "interface").Output()
	if strings.Contains(string(out), "phaethontun") {
		util.LogInfo("tun: disabling orphaned adapter phaethontun")
		if out2, err := exec.Command("netsh", "interface", "set", "interface", "phaethontun", "disable").CombinedOutput(); err != nil {
			util.LogWarn("tun: failed to disable adapter: %v, %s", err, out2)
		}
	}

	// Reset TUN interface DNS to DHCP
	if out, err := exec.Command("netsh", "interface", "ip", "set", "dns", "name=phaethontun", "dhcp").CombinedOutput(); err != nil {
		util.LogWarn("tun: dns reset fail: %v, %s", err, out)
	}

	// Reset physical interface DNS to DHCP as a crash-recovery best effort.
	// The original DNS backup is only available during normal Stop(); after a
	// crash we restore DHCP so the machine does not remain stuck on 198.18.0.1.
	if ifaceName := defaultGatewayInterfaceName(); ifaceName != "" {
		if out, err := exec.Command("netsh", "interface", "ip", "set", "dns", "name="+ifaceName, "dhcp").CombinedOutput(); err != nil {
			util.LogWarn("tun: restore dns for %s fail: %v, %s", ifaceName, err, out)
		} else {
			util.LogInfo("tun: restored dns for %s to dhcp", ifaceName)
		}
	}

	util.LogInfo("tun: residual cleanup completed")
}

// defaultGatewayInterfaceName returns the interface name of the IPv4 default
// route, or an empty string if it cannot be determined.
func defaultGatewayInterfaceName() string {
	out, err := exec.Command("netsh", "interface", "ip", "show", "route").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[3] != "0.0.0.0/0" {
			continue
		}
		var idx uint32
		if _, err := fmt.Sscanf(fields[4], "%d", &idx); err != nil {
			continue
		}
		if iface, err := net.InterfaceByIndex(int(idx)); err == nil {
			return iface.Name
		}
	}
	return ""
}
