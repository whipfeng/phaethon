//go:build windows

package tun

import (
	"fmt"
	"net"
	"unsafe"

	"phaethon/util"
)

func (r *RouteManager) platformSetup(tunIP string, prefixLen int) error {
	luid, index, err := getInterfaceLUID(r.devName)
	if err != nil {
		return err
	}

	// 1. Configure interface IP
	ip := net.ParseIP(tunIP).To4()
	if ip == nil {
		return fmt.Errorf("invalid tunIP: %s", tunIP)
	}
	var addrRow mibUnicastIpAddressRow
	addrRow.setAddress(ip)
	addrRow.setInterfaceLuid(luid)
	addrRow.setInterfaceIndex(index)
	addrRow.setOnLinkPrefixLength(uint8(prefixLen))

	ret, _, _ := procCreateUnicastIpAddressEntry.Call(uintptr(unsafe.Pointer(&addrRow[0])))
	if ret != 0 {
		if ret != 0x800704b0 {
			return fmt.Errorf("CreateUnicastIpAddressEntry: 0x%x", ret)
		}
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
	// server traffic bypasses TUN entirely.
	for _, ipStr := range r.excludeIPs {
		if ipStr == "" || ipStr == tunIP || r.originalGateway == nil {
			continue
		}
		var fwdRow mibIpForwardRow2
		fwdRow.init()
		fwdRow.setInterfaceLuid(gwLuid)
		fwdRow.setInterfaceIndex(gwIndex)
		fwdRow.setDestinationPrefix(net.ParseIP(ipStr), 32)
		fwdRow.setNextHop(r.originalGateway)
		fwdRow.setMetric(1)

		ret, _, _ = procCreateIpForwardEntry2.Call(uintptr(unsafe.Pointer(&fwdRow[0])))
		if ret != 0 {
			util.LogWarn("tun: add exclusion route %s fail: 0x%x", ipStr, ret)
		} else {
			util.LogInfo("tun: exclusion route %s -> %s", ipStr, r.originalGateway)
			r.appliedExcludes = append(r.appliedExcludes, ipStr)
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

		ret, _, _ = procCreateIpForwardEntry2.Call(uintptr(unsafe.Pointer(&fwdRow[0])))
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

	// Delete interface IP
	if r.tunIP != "" {
		tunIP := net.ParseIP(r.tunIP).To4()
		if tunIP != nil {
			var addrRow mibUnicastIpAddressRow
			addrRow.setAddress(tunIP)
			addrRow.setInterfaceLuid(luid)
			addrRow.setInterfaceIndex(index)
			addrRow.setOnLinkPrefixLength(15)
			procDeleteUnicastIpAddressEntry.Call(uintptr(unsafe.Pointer(&addrRow[0])))
		}
	}
}

// deleteExclusionRoute removes a /32 host route via the original interface.
func (r *RouteManager) deleteExclusionRoute(ip string) {
	if r.defaultIfaceLUID == 0 || r.originalGateway == nil {
		return
	}
	var fwdRow mibIpForwardRow2
	fwdRow.init()
	fwdRow.setInterfaceLuid(r.defaultIfaceLUID)
	fwdRow.setInterfaceIndex(uint32(r.DefaultIfaceIndex))
	fwdRow.setDestinationPrefix(net.ParseIP(ip), 32)
	fwdRow.setNextHop(r.originalGateway)
	fwdRow.setMetric(1)
	procDeleteIpForwardEntry2.Call(uintptr(unsafe.Pointer(&fwdRow[0])))
}
