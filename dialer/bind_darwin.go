//go:build darwin

package dialer

import (
	"net"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
	"phaethon/util"
)

func (b *BindContext) bindSocket(c syscall.RawConn, dst net.IP) error {
	idx := currentDefaultIndex(b)

	if dst != nil {
		if cached, ok := cachedRoute(dst, ""); ok {
			idx = indexFromCache(cached)
		} else {
			if bestIdx, err := darwinRouteIfaceIndex(dst, b.TUNIfaceName, idx); err == nil && bestIdx > 0 {
				idx = bestIdx
				setCachedRoute(dst, "", strconv.Itoa(idx))
			} else if err != nil {
				util.LogModuleDebug("bind", "dialer/bind: route lookup for %s failed: %v", dst, err)
			}
		}
	}

	if idx <= 0 {
		return nil
	}
	return setBoundIf(c, idx, dst)
}

// routeIfaceForDst returns the interface name for traffic to dst via a
// PF_ROUTE RTM_GET lookup, excluding the TUN interface.
func routeIfaceForDst(b *BindContext, dst net.IP) string {
	idx, err := darwinRouteIfaceIndex(dst, b.TUNIfaceName, b.DefaultIfaceIndex)
	if err != nil {
		return b.DefaultIfaceName
	}
	if iface, err := net.InterfaceByIndex(idx); err == nil {
		return iface.Name
	}
	return b.DefaultIfaceName
}

func setBoundIf(c syscall.RawConn, idx int, dst net.IP) error {
	var sockErr error
	err := c.Control(func(fd uintptr) {
		// Bind by the socket's actual address family: Go creates IPv6
		// dual-stack sockets for wildcard "udp" listens, where the IPv4-only
		// IP_BOUND_IF sockopt fails with EINVAL. Skipping family detection
		// and choosing by dst alone is what broke UDP relay on darwin.
		sa, err := unix.Getsockname(int(fd))
		if err != nil {
			sockErr = err
			return
		}
		switch sa.(type) {
		case *unix.SockaddrInet4:
			sockErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_BOUND_IF, idx)
		case *unix.SockaddrInet6:
			sockErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_BOUND_IF, idx)
		}
	})
	if err != nil {
		return err
	}
	return sockErr
}

// darwinRouteIfaceIndex returns the interface index for the best route to dst,
// excluding the TUN interface (PF_ROUTE RTM_GET, no subprocess).
func darwinRouteIfaceIndex(dst net.IP, tunIface string, fallbackIdx int) (int, error) {
	if dst == nil {
		return fallbackIdx, nil
	}
	idx, err := routeGetIfIndex(dst)
	if err != nil {
		return fallbackIdx, err
	}
	if tunIface != "" {
		if tun, err := net.InterfaceByName(tunIface); err == nil && tun.Index == idx {
			return fallbackIdx, nil
		}
	}
	return idx, nil
}
