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

	localIP := selectLocalIP(idx, dst)
	if localIP == nil {
		return nil
	}

	var bindErr error
	err := c.Control(func(fd uintptr) {
		if ip4 := localIP.To4(); ip4 != nil {
			sa := &syscall.SockaddrInet4{}
			copy(sa.Addr[:], ip4)
			bindErr = syscall.Bind(syscall.Handle(fd), sa)
		} else if ip16 := localIP.To16(); ip16 != nil {
			sa := &syscall.SockaddrInet6{}
			copy(sa.Addr[:], ip16)
			bindErr = syscall.Bind(syscall.Handle(fd), sa)
		}
	})
	if err != nil {
		return err
	}
	if bindErr != nil {
		util.LogWarn("dialer/bind: bind to %s (iface %d) failed: %v", localIP, idx, bindErr)
	}
	return bindErr
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
