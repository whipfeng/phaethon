//go:build windows

package tun

import (
	"net"
	"time"
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

		// Delete Fake-IP pool route for both the current off-link gateway
		// variant (192.0.2.1) and the legacy on-link variant (0.0.0.0).
		if _, fakeIPNet, err := net.ParseCIDR(FakeIPPoolCIDR); err == nil {
			for _, nh := range []net.IP{net.ParseIP("192.0.2.1").To4(), net.IPv4zero} {
				var fwdRow mibIpForwardRow2
				fwdRow.init()
				fwdRow.setInterfaceLuid(luid)
				fwdRow.setInterfaceIndex(index)
				fwdRow.setDestinationPrefix(fakeIPNet.IP, uint8(prefixLenFromMask(fakeIPNet.Mask)))
				fwdRow.setNextHop(nh)
				fwdRow.setMetric(1)
				procDeleteIpForwardEntry2.Call(uintptr(unsafe.Pointer(&fwdRow[0])))
			}
		}

		// Sweep the full route table for residual TUN routes that may not match
		// the specific combinations above (e.g. persistent routes detached from
		// the adapter LUID).
		deleteResidualRoutesAPI()

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
		_ = flushNeighborsByIPAPI(net.ParseIP("192.0.2.1").To4())
		_ = flushNeighborsByIPAPI(net.ParseIP("192.0.2.2").To4())
	}

	// Check if phaethontun adapter still exists and remove it. Disabling the
	// adapter leaves a dead Wintun interface in the system that prevents
	// subsequent CreateAdapter calls from creating a fresh adapter, so we
	// explicitly delete it.
	if InterfaceExists() {
		// Release any static IP/DNS first so the removal is clean.
		if luid, index, err := getInterfaceLUID("phaethontun"); err == nil {
			_ = clearInterfaceIPAPI(luid, index)
			_ = clearInterfaceDNSAPI(luid, index)
		}

		util.LogInfo("tun: removing orphaned adapter phaethontun")
		if err := removeAdapterAPI("phaethontun"); err != nil {
			util.LogWarn("tun: remove adapter failed: %v", err)
			if err2 := disableInterfaceAPI("phaethontun"); err2 != nil {
				util.LogWarn("tun: disable adapter fallback failed: %v", err2)
			}
		}

		// Wait until the adapter is actually gone; CreateAdapter can fail or
		// attach to a stale adapter if we proceed too quickly.
		for i := 0; i < 150; i++ {
			if _, _, err := getInterfaceLUID("phaethontun"); err != nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	// Reset physical interface DNS to DHCP as a crash-recovery best effort.
	// The original DNS backup is only available during normal Stop(); after a
	// crash we restore DHCP so the machine does not remain stuck on 198.18.0.1.
	if ifaceName := defaultGatewayInterfaceName(); ifaceName != "" {
		if gwLuid, _, err := getInterfaceLUID(ifaceName); err == nil {
			if gwIdx, err2 := luidToIndex(gwLuid); err2 == nil {
				if err3 := clearInterfaceDNSAPI(gwLuid, gwIdx); err3 != nil {
					util.LogWarn("tun: restore dns for %s fail: %v", ifaceName, err3)
				} else {
					util.LogInfo("tun: restored dns for %s to dhcp", ifaceName)
				}
			}
		}
	}

	util.LogInfo("tun: residual cleanup completed")
}

// defaultGatewayInterfaceName returns the interface name of the IPv4 default
// route, or an empty string if it cannot be determined.
func defaultGatewayInterfaceName() string {
	_, _, idx, err := getDefaultGatewayAPI()
	if err != nil {
		return ""
	}
	iface, err := net.InterfaceByIndex(int(idx))
	if err != nil {
		return ""
	}
	return iface.Name
}

// InterfaceExists reports whether the phaethontun network adapter is currently
// present in the TCP/IP stack.
func InterfaceExists() bool {
	_, _, err := getInterfaceLUID("phaethontun")
	return err == nil
}
