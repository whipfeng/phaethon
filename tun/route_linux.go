//go:build linux

package tun

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"

	"github.com/vishvananda/netlink"
	"phaethon/util"
)

func (r *RouteManager) platformSetup(tunIP string, prefixLen int) error {
	link, err := netlink.LinkByName(r.devName)
	if err != nil {
		return fmt.Errorf("link by name %s: %w", r.devName, err)
	}
	r.tunIndex = link.Attrs().Index

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

		// Capture original DNS servers before TUN redirects system DNS.
		if servers, err := readResolvConfNameservers(); err == nil {
			r.OriginalDNSServers = servers
			util.LogInfo("tun: original DNS servers: %v", servers)
		} else {
			util.LogWarn("tun: failed to capture original DNS servers: %v", err)
		}
	}

	// 4. Add exclusion routes (LAN/private subnets bypass TUN via original gateway)
	for _, exclude := range r.excludeIPs {
		if exclude == "" || exclude == tunIP || r.originalGateway == nil {
			continue
		}
		dst, err := parseExclusionLinux(exclude)
		if err != nil {
			util.LogWarn("tun: invalid exclusion %s: %v", exclude, err)
			continue
		}
		rt := &netlink.Route{
			Dst: dst,
			Gw:  r.originalGateway,
		}
		if err := netlink.RouteAdd(rt); err != nil {
			util.LogWarn("tun: add exclusion route %s fail: %v", exclude, err)
		} else {
			util.LogInfo("tun: exclusion route %s -> %s", exclude, r.originalGateway)
			r.appliedExcludes = append(r.appliedExcludes, exclude)
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

// parseExclusionLinux parses an exclusion entry into a destination IPNet.
func parseExclusionLinux(exclude string) (*net.IPNet, error) {
	if strings.Contains(exclude, "/") {
		_, ipNet, err := net.ParseCIDR(exclude)
		if err != nil {
			return nil, err
		}
		return ipNet, nil
	}
	ip := net.ParseIP(exclude)
	if ip == nil {
		return nil, fmt.Errorf("invalid IP: %s", exclude)
	}
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(32, 32)}, nil
}

func (r *RouteManager) deleteExclusionRoute(exclude string) {
	dst, err := parseExclusionLinux(exclude)
	if err != nil {
		util.LogWarn("tun: invalid exclusion to delete %s: %v", exclude, err)
		return
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

// readResolvConfNameservers returns the nameserver entries from /etc/resolv.conf.
func readResolvConfNameservers() ([]string, error) {
	f, err := os.Open("/etc/resolv.conf")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var servers []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "nameserver" {
			servers = append(servers, fields[1])
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return servers, nil
}
