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

	// 1. Configure interface IP via netsh (more reliable than iphlpapi on Wintun).
	// Clear any stale static IP first to avoid "object already exists" errors.
	_ = exec.Command("netsh", "interface", "ip", "set", "address", "name="+r.devName, "dhcp").Run()
	mask := net.IP(net.CIDRMask(prefixLen, 32)).String()
	cmd := exec.Command("netsh", "interface", "ip", "set", "address", "name="+r.devName, "static", tunIP, mask, "none")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("netsh set address: %v, %s", err, out)
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
	}

	// 3. Add exclusion routes via the ORIGINAL interface (not TUN).
	// These routes go through the original physical interface so proxy
	// server traffic and LAN/private subnets bypass TUN entirely.
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
	// so they take priority. The original default route remains for DIRECT
	// connections bound to the original interface.
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
	}

	return nil
}

func (r *RouteManager) platformTeardown() {
	luid, index, err := getInterfaceLUID(r.devName)
	if err != nil {
		return
	}

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
		util.LogWarn("tun: add exclusion route %s fail: 0x%x", exclude, ret)
		return false
	}
	util.LogInfo("tun: exclusion route %s -> %s", exclude, r.originalGateway)
	return true
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
