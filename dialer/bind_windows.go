//go:build windows

package dialer

import (
	"fmt"
	"net"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"phaethon/util"
)

var (
	modiphlpapi       = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetBestRoute2 = modiphlpapi.NewProc("GetBestRoute2")
)

// sockaddrInet represents the SOCKADDR_INET union (28 bytes).
type sockaddrInet [28]byte

func newSockaddrInet(ip net.IP) sockaddrInet {
	var s sockaddrInet
	if ip4 := ip.To4(); ip4 != nil {
		*(*uint16)(unsafe.Pointer(&s[0])) = windows.AF_INET
		copy(s[4:8], ip4)
	} else if ip16 := ip.To16(); ip16 != nil {
		*(*uint16)(unsafe.Pointer(&s[0])) = windows.AF_INET6
		copy(s[8:24], ip16)
	}
	return s
}

// mibIpForwardRow2Minimal is just large enough for GetBestRoute2 output.
type mibIpForwardRow2Minimal [104]byte

func (b *BindContext) bindSocket(c syscall.RawConn, dst net.IP) error {
	idx := currentDefaultIndex(b)

	if dst != nil {
		if cached, ok := cachedRoute(dst, ""); ok {
			idx = indexFromCache(cached)
		} else {
			if bestIdx, err := bestRouteIndex(dst, b.TUNLUID, uint32(idx)); err == nil {
				idx = int(bestIdx)
				setCachedRoute(dst, "", strconv.Itoa(idx))
			} else {
				util.LogDebug("dialer/bind: route lookup for %s failed: %v", dst, err)
			}
		}
	}

	if idx <= 0 {
		return nil
	}

	// IP_UNICAST_IF / IPV6_UNICAST_IF expect the interface index in network
	// byte order (host-to-network-long).
	idxNet := int(htonl(uint32(idx)))

	var sockErr error
	err := c.Control(func(fd uintptr) {
		// For IPv4 destinations or unknown, set IPv4 binding.
		if dst == nil || dst.To4() != nil {
			sockErr = windows.SetsockoptInt(windows.Handle(fd), windows.IPPROTO_IP, 31, idxNet)
			if sockErr != nil {
				return
			}
		}
		// For IPv6 destinations or unknown, also set IPv6 binding.
		if dst == nil || dst.To4() == nil {
			_ = windows.SetsockoptInt(windows.Handle(fd), windows.IPPROTO_IPV6, 31, idxNet)
		}
	})
	if err != nil {
		return err
	}
	if sockErr != nil {
		util.LogWarn("dialer/bind: IP_UNICAST_IF idx=%d(idxNet=%d) failed: %v", idx, idxNet, sockErr)
	}
	return sockErr
}

// bestRouteIndex returns the interface index for the best route to dst,
// excluding the TUN interface identified by excludeLuid.
func bestRouteIndex(dst net.IP, excludeLuid uint64, fallbackIdx uint32) (uint32, error) {
	dstAddr := newSockaddrInet(dst)
	var bestRoute mibIpForwardRow2Minimal
	var bestSrc sockaddrInet

	ret, _, _ := procGetBestRoute2.Call(
		0,
		0,
		0,
		uintptr(unsafe.Pointer(&dstAddr)),
		0,
		uintptr(unsafe.Pointer(&bestRoute[0])),
		uintptr(unsafe.Pointer(&bestSrc)),
	)
	if ret != 0 {
		return 0, fmt.Errorf("GetBestRoute2: 0x%x", ret)
	}

	luid := *(*uint64)(unsafe.Pointer(&bestRoute[0]))
	index := *(*uint32)(unsafe.Pointer(&bestRoute[8]))
	if luid == excludeLuid {
		if fallbackIdx == 0 {
			return 0, fmt.Errorf("best route leads to TUN and no fallback interface")
		}
		return fallbackIdx, nil
	}
	return index, nil
}
