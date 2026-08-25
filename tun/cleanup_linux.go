//go:build linux

package tun

import (
	"net"

	"github.com/vishvananda/netlink"
)

// CleanupResidual removes leftover routes from a previous abnormal exit.
func CleanupResidual() {
	link, err := netlink.LinkByName("tun0")
	if err != nil {
		return
	}

	// Delete split-tunnel routes (current scheme)
	for _, cidr := range []string{"0.0.0.0/1", "128.0.0.0/1"} {
		_, dst, _ := net.ParseCIDR(cidr)
		rt := &netlink.Route{LinkIndex: link.Attrs().Index, Dst: dst}
		_ = netlink.RouteDel(rt)
	}

	// Delete old-style default route (backward compat)
	rt := &netlink.Route{LinkIndex: link.Attrs().Index, Dst: nil}
	_ = netlink.RouteDel(rt)
}
