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
	// Use a /30 subnet. The adapter gets 192.0.2.2; the peer/gateway is 192.0.2.1.
	// The DNS hijacker lives on 127.0.0.1 inside the gVisor netstack and is reached
	// through the Windows-side DNS proxy listening on 192.0.2.2:53. We do not set a
	// default gateway via netsh; split-tunnel routes are added manually below.
	// Clear any stale static IP first to avoid "object already exists" errors.
	_ = exec.Command("netsh", "interface", "ip", "set", "address", "name="+r.devName, "dhcp").Run()
	adapterPrefixLen := prefixLen
	mask := net.IP(net.CIDRMask(adapterPrefixLen, 32)).String()
	tunPeerIP := net.ParseIP("192.0.2.1").To4()
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
	if out, err := exec.Command("netsh", "interface", "ipv4", "set", "interface", "name="+r.devName, "weakhostsend=enabled").CombinedOutput(); err != nil {
		util.LogWarn("tun: enable weakhostsend fail: %v, %s", err, out)
	}
	if out, err := exec.Command("netsh", "interface", "ipv4", "set", "interface", "name="+r.devName, "weakhostreceive=enabled").CombinedOutput(); err != nil {
		util.LogWarn("tun: enable weakhostreceive fail: %v, %s", err, out)
	}

	// 1b. Add a static neighbor for the virtual peer gateway 192.0.2.1 so Windows
	// does not try to ARP/NUD for it. The MAC is a placeholder; Wintun delivers
	// raw IP packets to user-mode regardless of the L2 address.
	if out, err := exec.Command("netsh", "interface", "ipv4", "add", "neighbor", "name="+r.devName, "address=192.0.2.1", "neighbor=00-00-00-00-00-01").CombinedOutput(); err != nil {
		util.LogWarn("tun: add static neighbor 192.0.2.1 fail: %v, %s", err, out)
	} else {
		util.LogInfo("tun: static neighbor 192.0.2.1 added")
	}

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
	// so they take priority. Use the virtual peer gateway (192.0.2.1) as next
	// hop; a static neighbor entry ensures Windows does not try to ARP for it.
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
		fwdRow.setNextHop(tunPeerIP)
		fwdRow.setMetric(1)

		ret, _, _ := procCreateIpForwardEntry2.Call(uintptr(unsafe.Pointer(&fwdRow[0])))
		if ret != 0 {
			return fmt.Errorf("CreateIpForwardEntry2 %s/%d: 0x%x", prefix.ip, prefix.len, ret)
		}
		util.LogInfo("tun: split-tunnel route %s/%d -> %s (luid=%x idx=%d)", prefix.ip, prefix.len, tunPeerIP, luid, index)
	}

	return nil
}

func (r *RouteManager) platformTeardown() {
	luid, index, err := getInterfaceLUID(r.devName)
	if err != nil {
		return
	}

	// The routes were created with the virtual peer gateway (192.0.2.1) as next hop.
	tunPeerIP := net.ParseIP("192.0.2.1").To4()

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
		fwdRow.setNextHop(tunPeerIP)
		fwdRow.setMetric(1)
		procDeleteIpForwardEntry2.Call(uintptr(unsafe.Pointer(&fwdRow[0])))
	}

	// Delete interface IP via netsh.
	_ = exec.Command("netsh", "interface", "ip", "set", "address", "name="+r.devName, "dhcp").Run()

	// Delete any default route netsh may have created via the adapter address.
	_ = exec.Command("route", "delete", "0.0.0.0", "mask", "0.0.0.0", "192.0.2.2").Run()
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
