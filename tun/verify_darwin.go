//go:build darwin

package tun

import (
	"fmt"
	"net"

	"phaethon/util"
)

// platformVerify checks the live Darwin address/route state against the cached
// setup. Probe failures are returned as OK so a transient routing-socket error
// cannot trigger a repair loop.
func (r *RouteManager) platformVerify(tunIP string) (TUNStateIssue, string) {
	iface, err := net.InterfaceByName(r.devName)
	if err != nil {
		return TUNStateInterfaceGone, fmt.Sprintf("TUN interface %s is gone: %v", r.devName, err)
	}
	if r.tunIndex != 0 && iface.Index != r.tunIndex {
		return TUNStateIdentityChanged,
			fmt.Sprintf("TUN interface %s was recreated (idx %d -> %d); cached fd is stale", r.devName, r.tunIndex, iface.Index)
	}

	ip := net.ParseIP(tunIP)
	if ip != nil {
		addrs, err := iface.Addrs()
		if err != nil {
			util.LogModuleWarn("route", "verify TUN address on %s failed, keeping TUN: %v", r.devName, err)
		} else {
			found := false
			for _, a := range addrs {
				if ipNet, ok := a.(*net.IPNet); ok && ipNet.IP.Equal(ip) {
					found = true
					break
				}
			}
			if !found {
				return TUNStateAddrMissing, fmt.Sprintf("TUN address %s missing on %s", ip, r.devName)
			}
		}
	}

	for _, probe := range []net.IP{net.IPv4(64, 0, 0, 1), net.IPv4(192, 0, 0, 1)} {
		idx, err := routeGetIfIndex(probe)
		if err != nil {
			util.LogModuleWarn("route", "verify split route via %s failed, keeping TUN: %v", probe, err)
			continue
		}
		if idx != iface.Index {
			return TUNStateRouteMissing,
				fmt.Sprintf("split route via %s now uses iface %d, expected %s (idx=%d)", probe, idx, r.devName, iface.Index)
		}
	}
	return TUNStateOK, ""
}

// platformReassert restores the TUN address and split routes on the same utun.
// It never recreates the device; an index change must be handled by a full
// recreate.
func (r *RouteManager) platformReassert(tunIP string, prefixLen int) error {
	iface, err := net.InterfaceByName(r.devName)
	if err != nil {
		return fmt.Errorf("TUN interface %s is gone: %w", r.devName, err)
	}
	if r.tunIndex != 0 && iface.Index != r.tunIndex {
		return fmt.Errorf("TUN interface %s was recreated (idx %d -> %d)", r.devName, r.tunIndex, iface.Index)
	}

	ip := net.ParseIP(tunIP)
	if ip == nil {
		return fmt.Errorf("invalid TUN IP %q", tunIP)
	}
	// Only re-add the address when it is actually missing: SIOCAIFADDR on a
	// live interface that already owns the address fails with EEXIST and would
	// abort the route repair below.
	hasAddr := false
	if addrs, err := iface.Addrs(); err == nil {
		for _, a := range addrs {
			if ipNet, ok := a.(*net.IPNet); ok && ipNet.IP.Equal(ip) {
				hasAddr = true
				break
			}
		}
	}
	if !hasAddr {
		if err := setIfaceAddr(r.devName, ip, prefixLen); err != nil {
			return fmt.Errorf("restore TUN address %s/%d: %w", ip, prefixLen, err)
		}
	}
	if err := setIfaceUp(r.devName, true); err != nil {
		return fmt.Errorf("bring %s up: %w", r.devName, err)
	}

	for _, cidr := range []string{"0.0.0.0/1", "128.0.0.0/1"} {
		_, dst, _ := net.ParseCIDR(cidr)
		if err := routeAddNetViaInterface(iface.Index, dst); err != nil && !isRouteExists(err) {
			return fmt.Errorf("restore split route %s: %w", cidr, err)
		}
	}

	if r.appliedIfscopeDefault && r.originalGateway != nil && r.DefaultIfaceIndex > 0 {
		if err := routeAddIfscopeDefault(r.originalGateway, r.DefaultIfaceIndex); err != nil && !isRouteExists(err) {
			util.LogModuleWarn("route", "restore ifscope default fail: %v", err)
		}
	}

	if r.originalGateway != nil {
		for _, exclude := range r.appliedExcludes {
			if exclude == "" || exclude == tunIP {
				continue
			}
			if err := r.addExclusionRoute(exclude); err != nil && !isRouteExists(err) {
				util.LogModuleWarn("route", "restore exclusion %s fail: %v", exclude, err)
			}
		}
	}

	util.LogModuleWarn("route", "re-applied TUN address/routes on %s without recreating the device", r.devName)
	return nil
}
