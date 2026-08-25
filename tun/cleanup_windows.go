//go:build windows

package tun

import (
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
		addrRow.setAddress(ip)
		addrRow.setInterfaceLuid(luid)
		addrRow.setInterfaceIndex(index)
		addrRow.setOnLinkPrefixLength(15)
		procDeleteUnicastIpAddressEntry.Call(uintptr(unsafe.Pointer(&addrRow[0])))
	}

	// Check if phaethontun adapter still exists and remove it
	out, _ := exec.Command("netsh", "interface", "show", "interface").Output()
	if strings.Contains(string(out), "phaethontun") {
		util.LogInfo("tun: removing orphaned adapter phaethontun")
		if out2, err := exec.Command("netsh", "interface", "delete", "interface", "name=phaethontun").CombinedOutput(); err != nil {
			util.LogWarn("tun: failed to remove adapter: %v, %s", err, out2)
		}
	}

	// Reset TUN interface DNS to DHCP
	if out, err := exec.Command("netsh", "interface", "ip", "set", "dns", "name=phaethontun", "dhcp").CombinedOutput(); err != nil {
		util.LogWarn("tun: dns reset fail: %v, %s", err, out)
	}
}
