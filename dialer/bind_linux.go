//go:build linux

package dialer

import (
	"net"
	"syscall"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func (b *BindContext) bindSocket(c syscall.RawConn, dst net.IP) error {
	iface := b.ResolveIface(dst)
	if iface == "" {
		return nil
	}
	return setBindToDevice(c, iface)
}

// routeIfaceForDst returns the interface name for traffic to dst by parsing
// /proc/net/route, excluding the TUN interface.
func routeIfaceForDst(b *BindContext, dst net.IP) string {
	if dst != nil && dst.To4() != nil {
		if bestIface, err := linuxRouteIface(dst.To4(), b.TUNIfaceName, b.DefaultIfaceName); err == nil {
			return bestIface
		}
	}
	return b.DefaultIfaceName
}

func setBindToDevice(c syscall.RawConn, iface string) error {
	var sockErr error
	err := c.Control(func(fd uintptr) {
		sockErr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, iface)
	})
	if err != nil {
		return err
	}
	return sockErr
}

// linuxRouteIface returns the interface name that should carry an IPv4 packet
// to dst by querying kernel routing tables via netlink. It checks both the
// local table (higher priority) and main table, excluding the TUN interface.
// Falls back to defaultIface if no route matches.
func linuxRouteIface(dst net.IP, tunIface, defaultIface string) (string, error) {
	if dst == nil {
		return defaultIface, nil
	}
	dst4 := dst.To4()
	if dst4 == nil {
		return defaultIface, nil
	}
	dstU := ipToUint32LE(dst4)

	var bestIface string
	bestPrefix := -1
	bestTable := -1 // Track which table the route came from (local=255, main=254)

	// Query local table first (higher priority)
	if localRoutes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{
		Table: syscall.RT_TABLE_LOCAL,
	}, netlink.RT_FILTER_TABLE); err == nil {
		for _, rt := range localRoutes {
			if rt.Dst == nil {
				continue
			}
			iface, prefix, ok := matchRoute(rt, dstU, tunIface)
			if !ok {
				continue
			}
			// Local table has higher priority, prefer it
			if bestTable < syscall.RT_TABLE_LOCAL || (bestTable == syscall.RT_TABLE_LOCAL && prefix > bestPrefix) {
				bestPrefix = prefix
				bestIface = iface
				bestTable = syscall.RT_TABLE_LOCAL
			}
		}
	}

	// Query main table
	if mainRoutes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{
		Table: syscall.RT_TABLE_MAIN,
	}, netlink.RT_FILTER_TABLE); err == nil {
		for _, rt := range mainRoutes {
			if rt.Dst == nil {
				continue
			}
			iface, prefix, ok := matchRoute(rt, dstU, tunIface)
			if !ok {
				continue
			}
			// Only use main table if we didn't find a local table match
			if bestTable < syscall.RT_TABLE_MAIN || (bestTable == syscall.RT_TABLE_MAIN && prefix > bestPrefix) {
				bestPrefix = prefix
				bestIface = iface
				bestTable = syscall.RT_TABLE_MAIN
			}
		}
	}

	if bestIface == "" {
		return defaultIface, nil
	}
	return bestIface, nil
}

// matchRoute checks if a route matches the destination and returns the interface,
// prefix length, and whether it matched.
func matchRoute(rt netlink.Route, dstU uint32, tunIface string) (string, int, bool) {
	// Get interface name
	iface, err := net.InterfaceByIndex(rt.LinkIndex)
	if err != nil {
		return "", 0, false
	}
	ifaceName := iface.Name

	// Skip TUN interface
	if ifaceName == tunIface {
		return "", 0, false
	}

	// Calculate prefix match
	ones, _ := rt.Dst.Mask.Size()
	mask := uint32(0)
	if ones > 0 {
		mask = ^uint32(0) << (32 - ones)
	}

	rtDstU := ipToUint32LE(rt.Dst.IP.To4())
	if dstU&mask != rtDstU&mask {
		return "", 0, false
	}

	return ifaceName, ones, true
}

// ipToUint32LE converts an IPv4 address to a uint32 in the same byte order as
// /proc/net/route (host / little-endian).
func ipToUint32LE(ip net.IP) uint32 {
	ip = ip.To4()
	return uint32(ip[0]) | uint32(ip[1])<<8 | uint32(ip[2])<<16 | uint32(ip[3])<<24
}
