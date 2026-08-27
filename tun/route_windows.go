//go:build windows

package tun

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"
	"unsafe"

	"phaethon/util"
)

func (r *RouteManager) platformSetup(tunIP string, prefixLen int) error {
	// The Wintun adapter may take a moment to register with the TCP/IP stack.
	// Retry LUID lookup briefly before giving up.
	var luid uint64
	var index uint32
	var err error
	for i := 0; i < 20; i++ {
		luid, index, err = getInterfaceLUID(r.devName)
		if err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		return err
	}
	r.tunLUID = luid

	// 1. Configure interface IP via netsh (more reliable than iphlpapi on Wintun).
	// Use a /32 address on the adapter so that the split-tunnel route next hop
	// (192.0.2.1) is off-link. With a /30 mask Windows normalizes the route to
	// on-link (next hop 0.0.0.0) and then tries to resolve a MAC for each Fake-IP
	// destination, which Wintun cannot do, causing unicast SYNs to be dropped.
	// The DNS hijacker lives on 127.0.0.1 inside the gVisor netstack and is reached
	// through the Windows-side DNS proxy listening on 192.0.2.2:53. We do not set a
	// default gateway via netsh; split-tunnel routes are added manually below.
	// Clear any stale static IP first to avoid "object already exists" errors.
	_ = exec.Command("netsh", "interface", "ip", "set", "address", "name="+r.devName, "dhcp").Run()
	adapterPrefixLen := 32
	mask := net.IP(net.CIDRMask(adapterPrefixLen, 32)).String()
	cmd := exec.Command("netsh", "interface", "ip", "set", "address", "name="+r.devName, "static", tunIP, mask, "none")
	out, err := cmd.CombinedOutput()
	util.LogInfo("tun: netsh set address output: %s (err=%v)", out, err)
	if err != nil {
		return fmt.Errorf("netsh set address: %v, %s", err, out)
	}

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
	deleteStaticNeighbors(r.devName, index)
	_ = deleteStaleNeighbors("192.0.2.1")
	_ = deleteStaleNeighbors("192.0.2.2")

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
			_, recv, err := getWeakHostState(r.DefaultIfaceName)
			if err == nil {
				r.originalPhysicalWeakHostReceive = recv
				util.LogInfo("tun: physical %s original weak-host receive: %v", r.DefaultIfaceName, recv)
			}
			if err := setWeakHost(r.DefaultIfaceName, "weakhostreceive", true); err != nil {
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
	// so they take priority. Use the virtual peer gateway 192.0.2.1 as next hop;
	// because the adapter is /32, this gateway is off-link and Windows sends
	// matching packets straight to the Wintun interface without ARP/NUD.
	tunGatewayIP := net.ParseIP("192.0.2.1").To4()
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

		ret, _, _ := procCreateIpForwardEntry2.Call(uintptr(unsafe.Pointer(&fwdRow[0])))
		if ret != 0 {
			return fmt.Errorf("CreateIpForwardEntry2 %s/%d: 0x%x", prefix.ip, prefix.len, ret)
		}
		util.LogInfo("tun: split-tunnel route %s/%d -> %s (luid=%x idx=%d)", prefix.ip, prefix.len, tunGatewayIP, luid, index)
	}

	return nil
}

func (r *RouteManager) platformTeardown() {
	luid, index, err := getInterfaceLUID(r.devName)
	if err != nil {
		return
	}

	// The routes were created with the virtual peer gateway 192.0.2.1 as next hop.
	tunGatewayIP := net.ParseIP("192.0.2.1").To4()

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

	// Also delete the on-link variant left by older builds (next hop 0.0.0.0).
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

	// Delete interface IP via netsh.
	_ = exec.Command("netsh", "interface", "ip", "set", "address", "name="+r.devName, "dhcp").Run()

	// Delete any default route netsh may have created via the adapter address.
	_ = exec.Command("route", "delete", "0.0.0.0", "mask", "0.0.0.0", "192.0.2.2").Run()

	// Delete the static neighbor for the virtual peer gateway so it does not
	// linger on the physical interface after the TUN adapter is gone.
	deleteStaticNeighbors(r.devName, index)
	_ = deleteStaleNeighbors("192.0.2.1")
	_ = deleteStaleNeighbors("192.0.2.2")

	// Restore physical interface weak-host receive to its original state.
	if r.DefaultIfaceName != "" {
		if err := setWeakHost(r.DefaultIfaceName, "weakhostreceive", r.originalPhysicalWeakHostReceive); err != nil {
			util.LogWarn("tun: restore physical weakhostreceive fail: %v", err)
		}
	}

	// Disable weak-host on the TUN adapter for cleanliness (best effort).
	_ = setWeakHost(r.devName, "weakhostsend", false)
	_ = setWeakHost(r.devName, "weakhostreceive", false)
}

// getWeakHostState queries weak-host send/receive state for the named interface.
// It recognizes both English and Chinese netsh output.
func getWeakHostState(name string) (send bool, recv bool, err error) {
	out, err := exec.Command("netsh", "interface", "ipv4", "show", "interfaces", "interface="+name, "level=verbose").CombinedOutput()
	if err != nil {
		return false, false, fmt.Errorf("netsh show interface %s: %w, %s", name, err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, ":") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		key := strings.ToLower(strings.TrimSpace(parts[0]))
		val := strings.ToLower(strings.TrimSpace(parts[1]))
		enabled := val == "enabled" || val == "已启用"
		if strings.Contains(key, "弱主机发送") || strings.Contains(key, "weak host sends") {
			send = enabled
		}
		if strings.Contains(key, "弱主机接收") || strings.Contains(key, "weak host receives") {
			recv = enabled
		}
	}
	return send, recv, nil
}

// setWeakHost sets a weak-host option on the named interface.
func setWeakHost(name, field string, enabled bool) error {
	val := "disabled"
	if enabled {
		val = "enabled"
	}
	out, err := exec.Command("netsh", "interface", "ipv4", "set", "interface", "interface="+name, field+"="+val).CombinedOutput()
	if err != nil {
		return fmt.Errorf("netsh set interface %s %s=%s: %w, %s", name, field, val, err, out)
	}
	return nil
}

// ensureWeakHostEnabled enables and verifies weak-host send/receive on the
// named interface, retrying briefly because Wintun registration can lag.
func ensureWeakHostEnabled(name string) error {
	for i := 0; i < 5; i++ {
		if err := setWeakHost(name, "weakhostsend", true); err != nil {
			return err
		}
		if err := setWeakHost(name, "weakhostreceive", true); err != nil {
			return err
		}
		send, recv, err := getWeakHostState(name)
		if err != nil {
			util.LogWarn("tun: unable to verify weak-host state on %s: %v", name, err)
			return nil
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

// deleteStaticNeighbors removes the static neighbor entry for the TUN peer
// gateway from the specified interface. It tolerates "element not found".
func deleteStaticNeighbors(name string, index uint32) {
	_ = exec.Command("netsh", "interface", "ipv4", "delete", "neighbors", "name="+name, "address=192.0.2.1").Run()
	_ = exec.Command("netsh", "interface", "ipv4", "delete", "neighbors", "name="+fmt.Sprintf("%d", index), "address=192.0.2.1").Run()
}

// deleteStaleNeighbors removes neighbor entries for the given IP from all
// interfaces. This prevents a residual entry on the physical NIC from hijacking
// traffic destined to the TUN peer gateway.
func deleteStaleNeighbors(ip string) error {
	out, err := exec.Command("powershell", "-NoProfile", "-Command",
		"Get-NetNeighbor | Where-Object { $_.IPAddress -eq '"+ip+"' } | ForEach-Object { netsh interface ipv4 delete neighbors name=$_.InterfaceIndex address='"+ip+"' store=active }").CombinedOutput()
	if err != nil {
		return fmt.Errorf("delete stale neighbors %s: %w, %s", ip, err, out)
	}
	return nil
}
