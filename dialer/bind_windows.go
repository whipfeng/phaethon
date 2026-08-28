//go:build windows

package dialer

import (
	"fmt"
	"net"
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

// bindSocket uses IP_UNICAST_IF to bind the socket to the interface that should
// carry traffic to dst. IP_UNICAST_IF works on all Windows adapter types
// (physical, VMware, Wintun, etc.) unlike syscall.Bind which fails in Control
// callbacks, and unlike LocalAddr which doesn't constrain routing on Windows
// (weak host model).
func (b *BindContext) bindSocket(c syscall.RawConn, dst net.IP) error {
	idx := currentDefaultIndex(b)

	if dst != nil {
		if bestIdx, err := bestRouteIndex(dst, b.TUNLUID, uint32(idx)); err == nil {
			idx = int(bestIdx)
		} else {
			util.LogDebug("dialer/bind: route lookup for %s failed: %v", dst, err)
		}
	}

	if idx <= 0 {
		return nil
	}

	var sockErr error
	err := c.Control(func(fd uintptr) {
		// IP_UNICAST_IF requires interface index in network byte order
		ifIndex := htonl(uint32(idx))
		sockErr = syscall.SetsockoptInt(
			syscall.Handle(fd),
			syscall.IPPROTO_IP,
			31, // IP_UNICAST_IF
			int(ifIndex),
		)
	})
	if err != nil {
		return err
	}
	if sockErr != nil {
		util.LogWarn("dialer/bind: IP_UNICAST_IF for iface %d failed: %v", idx, sockErr)
	}
	return sockErr
}

// htonl converts a uint32 from host to network byte order.
func htonl(val uint32) uint32 {
	return (val&0xFF)<<24 | (val&0xFF00)<<8 | (val&0xFF0000)>>8 | (val&0xFF000000)>>24
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
