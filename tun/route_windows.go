//go:build windows

package tun

import (
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"phaethon/util"
)

func (r *RouteManager) platformSetup(tunIP string, prefixLen int) error {
	// The Wintun adapter may take a moment to register with the TCP/IP stack.
	// Retry LUID lookup briefly before giving up.
	var luid uint64
	var index uint32
	var err error
	for i := 0; i < 80; i++ {
		luid, index, err = getInterfaceLUID(r.devName)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		return err
	}
	r.tunLUID = luid
	r.tunIndex = int(index)

	// 1. Configure interface IP via netsh (more reliable than iphlpapi on Wintun).
	// Use a /32 address on the adapter. The split-tunnel routes use the virtual
	// peer gateway 192.0.2.1 as next hop.
	// NOTE: System-wide TUN mode on Windows requires the Wintun adapter to
	// participate in the IP routing stack, but Wintun does not support ARP/NDP
	// (no link-layer address). This means Windows cannot resolve the gateway's
	// MAC address, and packets routed to the TUN interface are silently dropped.
	// A proper fix requires either using a TAP-Windows adapter (which has a MAC
	// address) or a WFP-based packet capture mechanism (e.g. WinDivert).
	// The DNS hijacker lives on 127.0.0.1 inside the gVisor netstack and is reached
	// through the Windows-side DNS proxy listening on 192.0.2.2:53. We do not set a
	// default gateway via netsh; split-tunnel routes are added manually below.
	// Clear any stale static IP first to avoid "object already exists" errors.
	_ = clearInterfaceIPAPI(luid, index)
	adapterPrefixLen := 32
	if err := setInterfaceIPAPI(luid, index, net.ParseIP(tunIP).To4(), uint8(adapterPrefixLen)); err != nil {
		return fmt.Errorf("set interface IP %s/%d: %w", tunIP, adapterPrefixLen, err)
	}
	util.LogInfo("tun: interface IP set to %s/%d via API (luid=%x idx=%d)", tunIP, adapterPrefixLen, luid, index)

	// 1a. Enable weak-host send/receive on the Wintun adapter. Wintun is a L3
	// tunnel; Windows' strong-host model drops packets whose source/destination
	// IPs are not assigned to the adapter. Weak-host allows the Fake-IP scheme to
	// work: outgoing SYNs have source 192.0.2.2 (local) but destination 198.18.x.x
	// (not local), and incoming replies have destination 192.0.2.2 (local) but
	// source 198.18.x.x (not local).
	if err := ensureWeakHostEnabled(r.devName); err != nil {
		util.LogWarn("tun: enable weak-host on %s fail: %v", r.devName, err)
	}

	// 1b. Clean up stale neighbors for the TUN-side IPs. We used to add a static
	// neighbor for 192.0.2.1, but Wintun adapters do not accept a link-layer
	// address for neighbor entries, which makes the entry useless for unicast
	// forwarding. Instead the split-tunnel routes below use 192.0.2.1 as an
	// off-link gateway on a /32 adapter, so Windows sends matching packets to
	// the Wintun interface without ARP/NUD.
	//
	// Do this synchronously before adding routes: a background goroutine that
	// deletes neighbor entries can race with route creation and invalidate the
	// off-link next hop before traffic starts.
	_ = deleteNeighborAPI(net.ParseIP("192.0.2.1").To4(), index, luid)
	_ = flushNeighborsByIPAPI(net.ParseIP("192.0.2.1").To4())
	_ = flushNeighborsByIPAPI(net.ParseIP("192.0.2.2").To4())

	// 2. Detect original default gateway and interface
	gw, gwLuid, gwIndex, err := getDefaultGatewayWindows()
	if err != nil {
		util.LogWarn("tun: failed to detect default gateway: %v", err)
	} else {
		r.originalGateway = gw
		r.defaultIfaceLUID = gwLuid
		r.DefaultIfaceIndex = int(gwIndex)
		if iface, err := net.InterfaceByIndex(int(gwIndex)); err == nil {
			r.DefaultIfaceName = iface.Name
		}
		util.LogInfo("tun: original gateway: %s (iface %s idx=%d luid=%x)", gw, r.DefaultIfaceName, gwIndex, gwLuid)

		// Capture original DNS servers before we redirect system DNS to TUN.
		if servers, err := getAdapterDNS(r.DefaultIfaceName); err == nil {
			r.OriginalDNSServers = servers
			util.LogInfo("tun: original DNS servers for %s: %v", r.DefaultIfaceName, servers)
		} else {
			util.LogWarn("tun: failed to capture original DNS servers: %v", err)
		}

		// Enable weak-host receive on the physical default interface. Replies
		// from the real Internet have destination 192.0.2.2 (the TUN adapter IP)
		// but arrive on the physical NIC, so the strong-host model drops them
		// unless weak-host receive is enabled there.
		if r.DefaultIfaceName != "" {
			_, recv, err := getWeakHostStateAPI(r.DefaultIfaceName)
			if err == nil {
				r.originalPhysicalWeakHostReceive = recv
				util.LogInfo("tun: physical %s original weak-host receive: %v", r.DefaultIfaceName, recv)
			}
			if err := setWeakHostReceiveAPI(r.DefaultIfaceName, true); err != nil {
				util.LogWarn("tun: enable physical weakhostreceive fail: %v", err)
			}
		}
	}

	// 3. Add exclusion routes via the ORIGINAL interface (not TUN).
	// These routes go through the original physical interface so LAN/private
	// subnets bypass TUN entirely.
	for _, exclude := range r.excludeIPs {
		if exclude == "" || exclude == tunIP || r.originalGateway == nil {
			continue
		}
		if r.addExclusionRoute(gwLuid, gwIndex, exclude) {
			r.appliedExcludes = append(r.appliedExcludes, exclude)
		}
	}

	// 4. Add split-tunnel routes (0.0.0.0/1 and 128.0.0.0/1) via TUN.
	// These are more specific than the original default route (0.0.0.0/0),
	// so they take priority. Use 0.0.0.0 (on-link) as next hop so that
	// Windows delivers packets directly to the Wintun driver without any
	// gateway/ARP resolution. Wintun is a layer 3 TUN adapter; it does not
	// have a link-layer address and cannot participate in ARP/NDP.
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

		ret, _, _ := procCreateIpForwardEntry2.Call(uintptr(unsafe.Pointer(&fwdRow[0])))
		if ret != 0 {
			return fmt.Errorf("CreateIpForwardEntry2 %s/%d: 0x%x", prefix.ip, prefix.len, ret)
		}
		util.LogInfo("tun: split-tunnel route %s/%d -> on-link (luid=%x idx=%d)", prefix.ip, prefix.len, luid, index)
	}

	// 5. Add an on-link route for the Fake-IP pool (198.18.0.0/15) via TUN.
	// The split-tunnel routes above already send 198.18.x.x to the TUN
	// interface, but this more-specific /15 route guarantees that bound
	// sockets (e.g. watchdog probes using IP_UNICAST_IF) see the destination as
	// directly attached on the TUN interface.
	_, fakeIPNet, err := net.ParseCIDR(FakeIPPoolCIDR)
	if err == nil {
		var fwdRow mibIpForwardRow2
		fwdRow.init()
		fwdRow.setInterfaceLuid(luid)
		fwdRow.setInterfaceIndex(index)
		fwdRow.setDestinationPrefix(fakeIPNet.IP, uint8(prefixLenFromMask(fakeIPNet.Mask)))
		fwdRow.setNextHop(net.IPv4zero)
		fwdRow.setMetric(1)

		ret, _, _ := procCreateIpForwardEntry2.Call(uintptr(unsafe.Pointer(&fwdRow[0])))
		if ret != 0 {
			util.LogWarn("tun: add Fake-IP pool route %s fail: 0x%x", FakeIPPoolCIDR, ret)
		} else {
			util.LogInfo("tun: Fake-IP pool route %s -> on-link (luid=%x idx=%d)", FakeIPPoolCIDR, luid, index)
		}
	}

	// 6. Wait until the adapter has the configured IPv4 address and the best route
	// to a public destination is via the TUN interface. This guarantees that when
	// Engine.Start() returns and the watchdog is spawned, probe traffic can
	// actually flow through TUN instead of failing because routes are not yet
	// propagated.
	if tunIPAddr := net.ParseIP(tunIP).To4(); tunIPAddr != nil {
		if err := waitForTUNReadiness(luid, index, tunIPAddr); err != nil {
			return fmt.Errorf("wait for TUN readiness: %w", err)
		}
	}

	// 7. Verify routes are actually in the routing table. If not, the interface
	// might have been recreated with a different index, making our routes invalid.
	if err := verifyRoutes(index); err != nil {
		return fmt.Errorf("route verification failed: %w", err)
	}

	return nil
}

// verifyRoutes checks that the split-tunnel routes are actually present in the
// routing table pointing to the expected interface index.
func verifyRoutes(expectedIndex uint32) error {
	iface, err := net.InterfaceByIndex(int(expectedIndex))
	if err != nil {
		return fmt.Errorf("interface %d not found: %w", expectedIndex, err)
	}

	_, routeIndex := bestRouteInterface(net.ParseIP("1.1.1.1").To4())
	if routeIndex != expectedIndex {
		return fmt.Errorf("best route to 1.1.1.1 uses interface %d (%s), expected %d",
			routeIndex, iface.Name, expectedIndex)
	}

	util.LogInfo("tun: route verification passed, best route to 1.1.1.1 uses interface %d (%s)",
		expectedIndex, iface.Name)
	return nil
}

// prefixLenFromMask returns the prefix length of an IPv4 mask.
func prefixLenFromMask(mask net.IPMask) int {
	ones, _ := mask.Size()
	return ones
}

// waitForTUNReadiness polls until the TUN adapter owns tunIP and the best route
// to a public destination (1.1.1.1) is via the TUN interface. This avoids the
// watchdog being spawned while Windows is still propagating the new routes.
func waitForTUNReadiness(luid uint64, index uint32, tunIP net.IP) error {
	const maxWait = 10 * time.Second
	const interval = 100 * time.Millisecond
	checkDst := net.ParseIP("1.1.1.1").To4()
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		if interfaceHasIPv4(int(index), tunIP) {
			rluid, ridx := bestRouteInterface(checkDst)
			if rluid == luid && ridx == index {
				util.LogInfo("tun: interface %d ready, best route to %s uses TUN", index, checkDst)
				return nil
			}
		}
		time.Sleep(interval)
	}
	return fmt.Errorf("TUN interface %d not ready after %v", index, maxWait)
}

// interfaceHasIPv4 reports whether the given interface has target as an IPv4
// unicast address.
func interfaceHasIPv4(index int, target net.IP) bool {
	iface, err := net.InterfaceByIndex(index)
	if err != nil {
		return false
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if ipNet, ok := a.(*net.IPNet); ok {
			if ip := ipNet.IP.To4(); ip != nil && ip.Equal(target) {
				return true
			}
		}
	}
	return false
}

// bestRouteInterface returns the interface LUID and index of the best route to
// dst by enumerating the forwarding table and finding the longest-prefix match.
// This is more reliable than GetBestRoute2, which can fail during adapter
// recreation on Windows 11.
func bestRouteInterface(dst net.IP) (uint64, uint32) {
	var table uintptr
	ret, _, _ := procGetIpForwardTable2.Call(
		uintptr(windows.AF_INET),
		uintptr(unsafe.Pointer(&table)),
	)
	if ret != 0 {
		return 0, 0
	}
	defer procFreeMibTable.Call(table)

	if table == 0 {
		return 0, 0
	}

	t := (*windows.MibIpForwardTable2)(unsafe.Pointer(table))
	rows := t.Rows()

	dst4 := dst.To4()
	if dst4 == nil {
		return 0, 0
	}

	var bestLuid uint64
	var bestIdx uint32
	bestPrefix := int32(-1)
	bestMetric := uint32(^uint32(0))

	for i := range rows {
		row := &rows[i]
		if row.DestinationPrefix.Prefix.Family != windows.AF_INET {
			continue
		}
		prefixLen := int32(row.DestinationPrefix.PrefixLength)
		rowAddr := (*windows.RawSockaddrInet4)(unsafe.Pointer(&row.DestinationPrefix.Prefix)).Addr
		rowUint := binary.BigEndian.Uint32(rowAddr[:])
		dstUint := binary.BigEndian.Uint32(dst4)

		var mask uint32
		if prefixLen > 0 {
			mask = ^uint32(0) << (32 - prefixLen)
		}
		if (dstUint & mask) != (rowUint & mask) {
			continue
		}

		if prefixLen > bestPrefix || (prefixLen == bestPrefix && row.Metric < bestMetric) {
			bestPrefix = prefixLen
			bestMetric = row.Metric
			bestLuid = row.InterfaceLuid
			bestIdx = row.InterfaceIndex
		}
	}
	return bestLuid, bestIdx
}


func (r *RouteManager) platformTeardown() {
	luid, index, err := getInterfaceLUID(r.devName)
	if err != nil {
		return
	}

	// The routes were created with on-link next hop (0.0.0.0). Also clean up the
	// legacy off-link gateway variant (192.0.2.1) left by older builds.
	for _, tunGatewayIP := range []net.IP{net.IPv4zero, net.ParseIP("192.0.2.1").To4()} {
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
			fwdRow.setNextHop(tunGatewayIP)
			fwdRow.setMetric(1)
			procDeleteIpForwardEntry2.Call(uintptr(unsafe.Pointer(&fwdRow[0])))
		}
	}

	// Delete the Fake-IP pool route for both the current off-link gateway
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

	// Delete interface IP.
	_ = clearInterfaceIPAPI(luid, index)

	// Delete any default route via 192.0.2.2 that netsh may have created.
	{
		var fwdRow mibIpForwardRow2
		fwdRow.init()
		fwdRow.setInterfaceLuid(luid)
		fwdRow.setInterfaceIndex(index)
		fwdRow.setDestinationPrefix(net.IPv4zero, 0)
		fwdRow.setNextHop(net.ParseIP("192.0.2.2").To4())
		procDeleteIpForwardEntry2.Call(uintptr(unsafe.Pointer(&fwdRow[0])))
	}

	// Delete the static neighbor for the virtual peer gateway so it does not
	// linger on the physical interface after the TUN adapter is gone.
	_ = deleteNeighborAPI(net.ParseIP("192.0.2.1").To4(), index, luid)
	_ = flushNeighborsByIPAPI(net.ParseIP("192.0.2.1").To4())
	_ = flushNeighborsByIPAPI(net.ParseIP("192.0.2.2").To4())

	// Restore physical interface weak-host receive to its original state.
	if r.DefaultIfaceName != "" {
		if err := setWeakHostReceiveAPI(r.DefaultIfaceName, r.originalPhysicalWeakHostReceive); err != nil {
			util.LogWarn("tun: restore physical weakhostreceive fail: %v", err)
		}
	}

	// Disable weak-host on the TUN adapter for cleanliness (best effort).
	_ = setWeakHostSendAPI(r.devName, false)
	_ = setWeakHostReceiveAPI(r.devName, false)
}

// ensureWeakHostEnabled enables and verifies weak-host send/receive on the
// named interface, retrying briefly because Wintun registration can lag.
func ensureWeakHostEnabled(name string) error {
	for i := 0; i < 10; i++ {
		if err := setWeakHostSendAPI(name, true); err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if err := setWeakHostReceiveAPI(name, true); err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		send, recv, err := getWeakHostStateAPI(name)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if send && recv {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	util.LogWarn("tun: weak-host send/receive could not be verified on %s after retries", name)
	return nil
}

// addExclusionRoute adds a host or CIDR route via the original interface.
// Returns true on success.
func (r *RouteManager) addExclusionRoute(gwLuid uint64, gwIndex uint32, exclude string) bool {
	ip, prefixLen, err := parseExclusionCIDR(exclude)
	if err != nil {
		util.LogWarn("tun: invalid exclusion %s: %v", exclude, err)
		return false
	}

	var fwdRow mibIpForwardRow2
	fwdRow.init()
	fwdRow.setInterfaceLuid(gwLuid)
	fwdRow.setInterfaceIndex(gwIndex)
	fwdRow.setDestinationPrefix(ip, prefixLen)
	fwdRow.setNextHop(r.originalGateway)
	fwdRow.setMetric(1)

	ret, _, _ := procCreateIpForwardEntry2.Call(uintptr(unsafe.Pointer(&fwdRow[0])))
	if ret != 0 {
		// 0x1392 (ERROR_OBJECT_ALREADY_EXISTS) means a residual route from a
		// previous crash is still present. Treat it as success so teardown will
		// delete it, and so the route is tracked in appliedExcludes.
		if ret == 0x1392 {
			util.LogDebug("tun: exclusion route %s already exists (residual)", exclude)
			return true
		}
		util.LogWarn("tun: add exclusion route %s fail: 0x%x", exclude, ret)
		return false
	}
	util.LogInfo("tun: exclusion route %s -> %s", exclude, r.originalGateway)
	return true
}

// deleteResidualExclusionRoute removes a host or CIDR route via the original
// interface using the exact fields our exclusion routes use. It is used by
// CleanupResidual to clear leftover routes from a crash.
func deleteResidualExclusionRoute(gwLuid uint64, gwIndex uint32, gateway net.IP, exclude string) {
	if gwLuid == 0 || gateway == nil {
		return
	}
	ip, prefixLen, err := parseExclusionCIDR(exclude)
	if err != nil {
		return
	}
	var fwdRow mibIpForwardRow2
	fwdRow.init()
	fwdRow.setInterfaceLuid(gwLuid)
	fwdRow.setInterfaceIndex(gwIndex)
	fwdRow.setDestinationPrefix(ip, prefixLen)
	fwdRow.setNextHop(gateway)
	fwdRow.setMetric(1)
	procDeleteIpForwardEntry2.Call(uintptr(unsafe.Pointer(&fwdRow[0])))
}

// parseExclusionCIDR parses an exclusion entry which may be a plain IP or CIDR.
func parseExclusionCIDR(exclude string) (net.IP, uint8, error) {
	if strings.Contains(exclude, "/") {
		_, ipNet, err := net.ParseCIDR(exclude)
		if err != nil {
			return nil, 0, err
		}
		ones, _ := ipNet.Mask.Size()
		return ipNet.IP, uint8(ones), nil
	}
	ip := net.ParseIP(exclude)
	if ip == nil {
		return nil, 0, fmt.Errorf("invalid IP: %s", exclude)
	}
	return ip, 32, nil
}

// deleteExclusionRoute removes a host or CIDR route via the original interface.
func (r *RouteManager) deleteExclusionRoute(exclude string) {
	if r.defaultIfaceLUID == 0 || r.originalGateway == nil {
		return
	}
	ip, prefixLen, err := parseExclusionCIDR(exclude)
	if err != nil {
		util.LogWarn("tun: invalid exclusion to delete %s: %v", exclude, err)
		return
	}
	var fwdRow mibIpForwardRow2
	fwdRow.init()
	fwdRow.setInterfaceLuid(r.defaultIfaceLUID)
	fwdRow.setInterfaceIndex(uint32(r.DefaultIfaceIndex))
	fwdRow.setDestinationPrefix(ip, prefixLen)
	fwdRow.setNextHop(r.originalGateway)
	fwdRow.setMetric(1)
	procDeleteIpForwardEntry2.Call(uintptr(unsafe.Pointer(&fwdRow[0])))
}


