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
		// Delete DNS host route (192.0.2.1/32) and split-tunnel routes for all
		// next-hop schemes we have used.
		for _, prefix := range []struct {
			ip  net.IP
			len uint8
		}{
			{net.ParseIP("192.0.2.1").To4(), 32},
			{net.ParseIP("0.0.0.0").To4(), 1},
			{net.ParseIP("128.0.0.0").To4(), 1},
		} {
			// Current /30 P2P scheme: next hop is the virtual peer gateway (192.0.2.1).
			var fwdRow mibIpForwardRow2
			fwdRow.init()
			fwdRow.setInterfaceLuid(luid)
			fwdRow.setInterfaceIndex(index)
			fwdRow.setDestinationPrefix(prefix.ip, prefix.len)
			fwdRow.setNextHop(net.ParseIP("192.0.2.2").To4())
			fwdRow.setMetric(0)
			fwdRow.setMetric(1)
			procDeleteIpForwardEntry2.Call(uintptr(unsafe.Pointer(&fwdRow[0])))

			// Current /30 P2P scheme: next hop is the virtual peer gateway (192.0.2.1).
			fwdRow.setNextHop(net.ParseIP("192.0.2.1").To4())
			fwdRow.setMetric(0)
			fwdRow.setMetric(1)
			procDeleteIpForwardEntry2.Call(uintptr(unsafe.Pointer(&fwdRow[0])))

			// /31 peer gateway default route.
			fwdRow.setNextHop(net.ParseIP("192.0.2.3").To4())
			fwdRow.setMetric(0)
			fwdRow.setMetric(1)
			procDeleteIpForwardEntry2.Call(uintptr(unsafe.Pointer(&fwdRow[0])))

			// Previous scheme: next hop was the TUN interface IP (198.18.0.1).
			fwdRow.setNextHop(net.ParseIP("198.18.0.1").To4())
			fwdRow.setMetric(0)
			fwdRow.setMetric(1)
			procDeleteIpForwardEntry2.Call(uintptr(unsafe.Pointer(&fwdRow[0])))

			// Previous scheme: next hop was the DNS hijacker IP (198.18.0.2).
			fwdRow.setNextHop(net.ParseIP("198.18.0.2").To4())
			fwdRow.setMetric(0)
			fwdRow.setMetric(1)
			procDeleteIpForwardEntry2.Call(uintptr(unsafe.Pointer(&fwdRow[0])))

			// Metric 1 variant (older /30 P2P builds).
			fwdRow.setMetric(1)
			procDeleteIpForwardEntry2.Call(uintptr(unsafe.Pointer(&fwdRow[0])))

			// Old scheme (gateway 0.0.0.0) for backward compatibility.
			fwdRow.setNextHop(net.IPv4zero)
			fwdRow.setMetric(1)
			procDeleteIpForwardEntry2.Call(uintptr(unsafe.Pointer(&fwdRow[0])))
		}

		// Delete default routes via either TUN gateway or adapter IP for all
		// metric variants that netsh/route may create.
		for _, metric := range []uint32{0, 1, 5, 6, 10, 50, 100} {
			var fwdRow mibIpForwardRow2
			fwdRow.init()
			fwdRow.setInterfaceLuid(luid)
			fwdRow.setInterfaceIndex(index)
			fwdRow.setDestinationPrefix(net.IPv4zero, 0)
			fwdRow.setNextHop(net.ParseIP("192.0.2.1").To4())
			fwdRow.setMetric(metric)
			procDeleteIpForwardEntry2.Call(uintptr(unsafe.Pointer(&fwdRow[0])))
			fwdRow.setNextHop(net.ParseIP("192.0.2.2").To4())
			procDeleteIpForwardEntry2.Call(uintptr(unsafe.Pointer(&fwdRow[0])))
			fwdRow.setNextHop(net.ParseIP("192.0.2.3").To4())
			procDeleteIpForwardEntry2.Call(uintptr(unsafe.Pointer(&fwdRow[0])))
			fwdRow.setNextHop(net.ParseIP("198.18.0.1").To4())
			procDeleteIpForwardEntry2.Call(uintptr(unsafe.Pointer(&fwdRow[0])))
			fwdRow.setNextHop(net.ParseIP("198.18.0.2").To4())
			procDeleteIpForwardEntry2.Call(uintptr(unsafe.Pointer(&fwdRow[0])))
		}

		// Fallback: netsh/route delete catches persistent default routes that
		// may not be attached to the adapter LUID after the adapter is gone.
		_ = exec.Command("route", "delete", "0.0.0.0", "mask", "0.0.0.0", "192.0.2.1").Run()
		_ = exec.Command("route", "delete", "0.0.0.0", "mask", "0.0.0.0", "192.0.2.2").Run()
		_ = exec.Command("route", "delete", "0.0.0.0", "mask", "0.0.0.0", "192.0.2.3").Run()
		_ = exec.Command("route", "delete", "0.0.0.0", "mask", "0.0.0.0", "198.18.0.1").Run()
		_ = exec.Command("route", "delete", "0.0.0.0", "mask", "0.0.0.0", "198.18.0.2").Run()

		// Remove residual exclusion routes left by a previous abnormal exit.
	// These routes point at the original physical gateway with metric 1 and
	// would otherwise make the next Setup() fail with ERROR_OBJECT_ALREADY_EXISTS.
	if gw, gwLuid, gwIdx, err := getDefaultGatewayWindows(); err == nil {
		for _, exclude := range DefaultLANExclusions {
			deleteResidualExclusionRoute(gwLuid, gwIdx, gw, exclude)
		}
	}

	// Delete interface addresses for current /30 scheme and legacy prefixes.
		for _, ip := range []net.IP{net.ParseIP("192.0.2.2").To4(), net.ParseIP("192.0.2.1").To4(), net.ParseIP("198.18.0.1").To4()} {
			for _, prefixLen := range []uint8{31, 32, 30, 24, 15} {
				var addrRow mibUnicastIpAddressRow
				addrRow.init()
				addrRow.setAddress(ip)
				addrRow.setInterfaceLuid(luid)
				addrRow.setInterfaceIndex(index)
				addrRow.setOnLinkPrefixLength(prefixLen)
				procDeleteUnicastIpAddressEntry.Call(uintptr(unsafe.Pointer(&addrRow[0])))
			}
		}

		// Delete any static neighbor entries we added for the virtual peer gateway.
		_ = exec.Command("netsh", "interface", "ipv4", "delete", "neighbors", "name=phaethontun").Run()
		_ = deleteStaleNeighbors("192.0.2.1")
		_ = deleteStaleNeighbors("192.0.2.2")
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

// InterfaceExists reports whether the phaethontun network adapter is currently
// present in the TCP/IP stack.
func InterfaceExists() bool {
	_, _, err := getInterfaceLUID("phaethontun")
	return err == nil
}
