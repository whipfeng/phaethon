//go:build darwin

package dialer

import (
	"net"
	"os/exec"
	"strconv"
	"strings"
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
				util.LogDebug("dialer/bind: route lookup for %s failed: %v", dst, err)
			}
		}
	}

	if idx <= 0 {
		return nil
	}
	return setBoundIf(c, idx, dst)
}

func setBoundIf(c syscall.RawConn, idx int, dst net.IP) error {
	var sockErr error
	err := c.Control(func(fd uintptr) {
		if dst == nil || dst.To4() != nil {
			sockErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_BOUND_IF, idx)
			if sockErr != nil {
				return
			}
		}
		if dst == nil || dst.To4() == nil {
			_ = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_BOUND_IF, idx)
		}
	})
	if err != nil {
		return err
	}
	return sockErr
}

// darwinRouteIfaceIndex returns the interface index for the best route to dst,
// excluding the TUN interface. It shells out to `route -n get`.
func darwinRouteIfaceIndex(dst net.IP, tunIface string, fallbackIdx int) (int, error) {
	if dst == nil {
		return fallbackIdx, nil
	}
	out, err := exec.Command("route", "-n", "get", dst.String()).Output()
	if err != nil {
		return fallbackIdx, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "interface:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		ifaceName := fields[1]
		if ifaceName == tunIface {
			return fallbackIdx, nil
		}
		if iface, err := net.InterfaceByName(ifaceName); err == nil {
			return iface.Index, nil
		}
	}
	return fallbackIdx, nil
}
