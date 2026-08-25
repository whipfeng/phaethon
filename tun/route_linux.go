//go:build linux

package tun

import (
	"errors"
	"fmt"
	"net"
	"syscall"

	"github.com/vishvananda/netlink"
	"phaethon/util"
)

func (r *RouteManager) platformSetup(tunIP string, prefixLen int) error {
	link, err := netlink.LinkByName(r.devName)
	if err != nil {
		return fmt.Errorf("link by name %s: %w", r.devName, err)
	}

	// 1. Set TUN interface IP
	ipNet := &net.IPNet{
		IP:   net.ParseIP(tunIP),
		Mask: net.CIDRMask(prefixLen, 32),
	}
	addr := &netlink.Addr{
		IPNet: ipNet,
	}
	if err := netlink.AddrAdd(link, addr); err != nil {
		if !isExist(err) {
			return fmt.Errorf("addr add: %w", err)
		}
	}

	// 2. Bring interface up
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("link set up: %w", err)
	}

	// 3. Detect original default gateway and interface
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		util.LogWarn("tun: failed to list routes: %v", err)
	} else {
		for _, route := range routes {
			if route.Dst == nil && route.Gw != nil {
				r.originalGateway = route.Gw
				if ifaceLink, err := netlink.LinkByIndex(route.LinkIndex); err == nil {
					r.DefaultIfaceName = ifaceLink.Attrs().Name
					r.DefaultIfaceIndex = ifaceLink.Attrs().Index
				}
				util.LogInfo("tun: original gateway: %s (iface=%s idx=%d)", r.originalGateway, r.DefaultIfaceName, r.DefaultIfaceIndex)
				break
			}
		}
	}

	// 4. Add exclusion routes (proxy server IPs bypass TUN via original gateway)
	for _, ip := range r.excludeIPs {
		if ip == "" || ip == tunIP || r.originalGateway == nil {
			continue
		}
		dst := &net.IPNet{
			IP:   net.ParseIP(ip),
			Mask: net.CIDRMask(32, 32),
		}
		rt := &netlink.Route{
			Dst: dst,
			Gw:  r.originalGateway,
		}
		if err := netlink.RouteAdd(rt); err != nil {
			util.LogWarn("tun: add exclusion route %s fail: %v", ip, err)
		} else {
			util.LogInfo("tun: exclusion route %s/32 -> %s", ip, r.originalGateway)
			r.appliedExcludes = append(r.appliedExcludes, ip)
		}
	}

	// 5. Add split-tunnel routes (0.0.0.0/1 and 128.0.0.0/1) via TUN.
	// These are more specific than the original default route (0.0.0.0/0),
	// so they take priority. The original default route remains in place for
	// DIRECT connections that bind to the original interface.
	for _, cidr := range []string{"0.0.0.0/1", "128.0.0.0/1"} {
		_, dst, _ := net.ParseCIDR(cidr)
		rt := &netlink.Route{
			LinkIndex: link.Attrs().Index,
			Dst:       dst,
		}
		if err := netlink.RouteAdd(rt); err != nil {
			return fmt.Errorf("route add %s: %w", cidr, err)
		}
	}

	return nil
}

func (r *RouteManager) platformTeardown() {
	link, err := netlink.LinkByName(r.devName)
	if err != nil {
		return
	}
	for _, cidr := range []string{"0.0.0.0/1", "128.0.0.0/1"} {
		_, dst, _ := net.ParseCIDR(cidr)
		rt := &netlink.Route{
			LinkIndex: link.Attrs().Index,
			Dst:       dst,
		}
		_ = netlink.RouteDel(rt)
	}
}

func (r *RouteManager) deleteExclusionRoute(ip string) {
	dst := &net.IPNet{
		IP:   net.ParseIP(ip),
		Mask: net.CIDRMask(32, 32),
	}
	rt := &netlink.Route{
		Dst: dst,
		Gw:  r.originalGateway,
	}
	_ = netlink.RouteDel(rt)
}

func isExist(err error) bool {
	return errors.Is(err, syscall.EEXIST)
}
