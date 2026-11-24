//go:build linux

package tun

import (
	"fmt"
	"net"

	"github.com/vishvananda/netlink"

	"phaethon/util"
)

// platformVerify checks the live Linux address/route state against the cached
// setup. Enumeration failures are returned as OK so a transient netlink error
// cannot trigger a repair loop.
func (r *RouteManager) platformVerify(tunIP string) (TUNStateIssue, string) {
	link, err := netlink.LinkByName(r.devName)
	if err != nil {
		return TUNStateInterfaceGone, fmt.Sprintf("TUN link %s is gone: %v", r.devName, err)
	}
	idx := link.Attrs().Index
	if r.tunIndex != 0 && idx != r.tunIndex {
		return TUNStateIdentityChanged,
			fmt.Sprintf("TUN link %s was recreated (idx %d -> %d); cached fd is stale", r.devName, r.tunIndex, idx)
	}

	ip := net.ParseIP(tunIP)
	if ip != nil {
		addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
		if err != nil {
			util.LogModuleWarn("route", "verify TUN address on %s failed, keeping TUN: %v", r.devName, err)
		} else {
			found := false
			for _, a := range addrs {
				if a.IP.Equal(ip) {
					found = true
					break
				}
			}
			if !found {
				return TUNStateAddrMissing, fmt.Sprintf("TUN address %s missing on %s", ip, r.devName)
			}
		}
	}

	routes, err := netlink.RouteList(link, netlink.FAMILY_V4)
	if err != nil {
		util.LogModuleWarn("route", "verify TUN routes on %s failed, keeping TUN: %v", r.devName, err)
		return TUNStateOK, ""
	}
	for _, cidr := range []string{"0.0.0.0/1", "128.0.0.0/1"} {
		_, dst, _ := net.ParseCIDR(cidr)
		if !routeOnLink(routes, dst) {
			return TUNStateRouteMissing, fmt.Sprintf("split route %s missing on %s", cidr, r.devName)
		}
	}
	return TUNStateOK, ""
}

func routeOnLink(routes []netlink.Route, dst *net.IPNet) bool {
	wantOnes, _ := dst.Mask.Size()
	for i := range routes {
		rt := &routes[i]
		if rt.Dst == nil || !rt.Dst.IP.Equal(dst.IP) {
			continue
		}
		if ones, _ := rt.Dst.Mask.Size(); ones == wantOnes {
			return true
		}
	}
	return false
}

// platformReassert restores the TUN address and split routes on the same link.
// It never recreates the device; an index change must be handled by a full
// recreate.
func (r *RouteManager) platformReassert(tunIP string, prefixLen int) error {
	link, err := netlink.LinkByName(r.devName)
	if err != nil {
		return fmt.Errorf("TUN link %s is gone: %w", r.devName, err)
	}
	idx := link.Attrs().Index
	if r.tunIndex != 0 && idx != r.tunIndex {
		return fmt.Errorf("TUN link %s was recreated (idx %d -> %d)", r.devName, r.tunIndex, idx)
	}

	ip := net.ParseIP(tunIP)
	if ip == nil {
		return fmt.Errorf("invalid TUN IP %q", tunIP)
	}
	addr := &netlink.Addr{IPNet: &net.IPNet{IP: ip, Mask: net.CIDRMask(prefixLen, 32)}}
	if err := netlink.AddrReplace(link, addr); err != nil {
		return fmt.Errorf("restore TUN address %s/%d: %w", ip, prefixLen, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bring %s up: %w", r.devName, err)
	}

	for _, cidr := range []string{"0.0.0.0/1", "128.0.0.0/1"} {
		_, dst, _ := net.ParseCIDR(cidr)
		rt := &netlink.Route{
			LinkIndex: idx,
			Dst:       dst,
			Protocol:  routeProtocolPhaethon,
		}
		if err := netlink.RouteReplace(rt); err != nil {
			return fmt.Errorf("restore split route %s: %w", cidr, err)
		}
	}

	if r.originalGateway != nil {
		for _, exclude := range r.appliedExcludes {
			if exclude == "" || exclude == tunIP {
				continue
			}
			dst, err := parseExclusionLinux(exclude)
			if err != nil {
				continue
			}
			_ = netlink.RouteReplace(&netlink.Route{
				Dst:      dst,
				Gw:       r.originalGateway,
				Protocol: routeProtocolPhaethon,
			})
		}
	}

	util.LogModuleWarn("route", "re-applied TUN address/routes on %s without recreating the device", r.devName)
	return nil
}
