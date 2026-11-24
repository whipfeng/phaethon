//go:build windows

package tun

import (
	"encoding/binary"
	"fmt"
	"net"
	"unsafe"

	"golang.org/x/sys/windows"

	"phaethon/util"
)

// platformVerify checks the live Windows routing/interface state against the
// cached setup. It deliberately treats enumeration failures as "unknown" (OK)
// so a transient API error cannot trigger a repair loop.
func (r *RouteManager) platformVerify(tunIP string) (TUNStateIssue, string) {
	luid, index, err := getInterfaceLUID(r.devName)
	if err != nil {
		return TUNStateInterfaceGone, fmt.Sprintf("TUN interface %s is gone: %v", r.devName, err)
	}
	if r.tunLUID != 0 && luid != r.tunLUID {
		return TUNStateIdentityChanged,
			fmt.Sprintf("TUN adapter instance changed (luid %x -> %x); cached session is stale", r.tunLUID, luid)
	}

	ip := net.ParseIP(tunIP).To4()
	if ip != nil && !interfaceHasIPv4(int(index), ip) {
		return TUNStateAddrMissing, fmt.Sprintf("TUN address %s/32 missing on %s", ip, r.devName)
	}

	for _, prefix := range []struct {
		ip  net.IP
		len uint8
	}{
		{net.ParseIP("0.0.0.0").To4(), 1},
		{net.ParseIP("128.0.0.0").To4(), 1},
	} {
		exists, err := forwardEntryExists(luid, prefix.ip, prefix.len)
		if err != nil {
			util.LogModuleWarn("route", "verify split route %s/%d failed, keeping TUN: %v", prefix.ip, prefix.len, err)
			continue
		}
		if !exists {
			return TUNStateRouteMissing, fmt.Sprintf("split route %s/%d missing on %s", prefix.ip, prefix.len, r.devName)
		}
	}

	// A foreign route can keep both split entries but win the lookup with a
	// lower metric; verify the actual best route too. Index 0 means the table
	// could not be enumerated, not that the route is absent.
	if _, bestIdx := bestRouteInterface(net.ParseIP("1.1.1.1").To4()); bestIdx != 0 && bestIdx != index {
		return TUNStateRouteMissing,
			fmt.Sprintf("best route to 1.1.1.1 no longer uses %s (idx=%d, expected %d)", r.devName, bestIdx, index)
	}

	return TUNStateOK, ""
}

// platformReassert restores the TUN address and split routes on the same
// adapter instance. It never touches the Wintun session; an identity change
// must be handled by a full recreate.
func (r *RouteManager) platformReassert(tunIP string, prefixLen int) error {
	luid, index, err := getInterfaceLUID(r.devName)
	if err != nil {
		return fmt.Errorf("TUN interface %s is gone: %w", r.devName, err)
	}
	if r.tunLUID != 0 && luid != r.tunLUID {
		return fmt.Errorf("TUN adapter instance changed (luid %x -> %x)", r.tunLUID, luid)
	}

	ip := net.ParseIP(tunIP).To4()
	if ip == nil {
		return fmt.Errorf("invalid TUN IP %q", tunIP)
	}
	if !interfaceHasIPv4(int(index), ip) {
		if err := setInterfaceIPAPI(luid, index, ip, 32); err != nil {
			return fmt.Errorf("restore interface IP %s/32: %w", ip, err)
		}
		util.LogModuleWarn("route", "restored TUN address %s/32 on %s", ip, r.devName)
	}

	// The adapter may have been reset without recreating it; weak-host is part
	// of the data path (fake-IP source/destination are not adapter-local).
	if err := ensureWeakHostEnabled(r.devName); err != nil {
		util.LogModuleWarn("route", "re-assert weak-host on %s fail: %v", r.devName, err)
	}

	for _, prefix := range []struct {
		ip  net.IP
		len uint8
	}{
		{net.ParseIP("0.0.0.0").To4(), 1},
		{net.ParseIP("128.0.0.0").To4(), 1},
	} {
		exists, err := forwardEntryExists(luid, prefix.ip, prefix.len)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		var fwdRow mibIpForwardRow2
		fwdRow.init()
		fwdRow.setInterfaceLuid(luid)
		fwdRow.setInterfaceIndex(index)
		fwdRow.setDestinationPrefix(prefix.ip, prefix.len)
		fwdRow.setNextHop(net.IPv4zero)
		fwdRow.setMetric(1)
		ret, _, _ := procCreateIpForwardEntry2.Call(uintptr(unsafe.Pointer(&fwdRow[0])))
		if ret != 0 && ret != 0x1392 { // 0x1392: ERROR_OBJECT_ALREADY_EXISTS
			return fmt.Errorf("restore split route %s/%d: 0x%x", prefix.ip, prefix.len, ret)
		}
		util.LogModuleWarn("route", "restored split route %s/%d on %s", prefix.ip, prefix.len, r.devName)
	}

	// Exclusions are part of the split-tunnel contract: without them LAN and
	// multicast traffic would be pulled into TUN by the 0/1 routes.
	if r.originalGateway != nil && r.defaultIfaceLUID != 0 {
		for _, exclude := range r.appliedExcludes {
			if exclude == "" || exclude == tunIP {
				continue
			}
			_ = r.addExclusionRoute(r.defaultIfaceLUID, uint32(r.DefaultIfaceIndex), exclude)
		}
	}

	return nil
}

// forwardEntryExists reports whether the forwarding table contains the given
// prefix on the expected interface. An enumeration error is returned to the
// caller instead of being treated as absence.
func forwardEntryExists(luid uint64, ip net.IP, prefixLen uint8) (bool, error) {
	ip4 := ip.To4()
	if ip4 == nil {
		return false, fmt.Errorf("not an IPv4 prefix: %v", ip)
	}
	var table uintptr
	ret, _, _ := procGetIpForwardTable2.Call(
		uintptr(windows.AF_INET),
		uintptr(unsafe.Pointer(&table)),
	)
	if ret != 0 {
		return false, fmt.Errorf("GetIpForwardTable2: 0x%x", ret)
	}
	if table == 0 {
		return false, nil
	}
	defer procFreeMibTable.Call(table)

	want := binary.BigEndian.Uint32(ip4)
	t := (*windows.MibIpForwardTable2)(unsafe.Pointer(table))
	rows := t.Rows()
	for i := range rows {
		row := &rows[i]
		if row.InterfaceLuid != luid || row.DestinationPrefix.Prefix.Family != windows.AF_INET {
			continue
		}
		if row.DestinationPrefix.PrefixLength != prefixLen {
			continue
		}
		addr := (*windows.RawSockaddrInet4)(unsafe.Pointer(&row.DestinationPrefix.Prefix)).Addr
		if binary.BigEndian.Uint32(addr[:]) == want {
			return true, nil
		}
	}
	return false, nil
}
