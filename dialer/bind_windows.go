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

// bindSocket is a no-op on Windows. Windows uses localAddr() with
// net.Dialer.LocalAddr instead of Control+Bind, because syscall.Bind inside
// Control callbacks fails with "invalid argument" on all interfaces.
func (b *BindContext) bindSocket(c syscall.RawConn, dst net.IP) error {
	return nil
}

// localAddr returns the local address to bind to for traffic to dst. On
// Windows, this is the correct way to bind sockets to a specific interface:
// set the source IP in the dialer/listener, and the routing stack
// deterministically selects the interface that owns that IP.
func (b *BindContext) localAddr(network string, dst net.IP) net.Addr {
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

	// Return the appropriate address type based on network.
	if network == "udp" || network == "udp4" || network == "udp6" {
		return &net.UDPAddr{IP: localIP}
	}
	return &net.TCPAddr{IP: localIP}
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
