//go:build linux

package dialer

import (
	"bufio"
	"math/bits"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
	"phaethon/util"
)

func (b *BindContext) bindSocket(c syscall.RawConn, dst net.IP) error {
	iface := b.DefaultIfaceName
	if dst != nil && dst.To4() != nil {
		if cached, ok := cachedRoute(dst, ""); ok {
			iface = cached
		} else {
			if bestIface, err := linuxRouteIface(dst.To4(), b.TUNIfaceName, b.DefaultIfaceName); err == nil && bestIface != "" {
				iface = bestIface
				setCachedRoute(dst, "", iface)
			} else if err != nil {
				util.LogDebug("dialer/bind: route lookup for %s failed: %v", dst, err)
			}
		}
	}

	if iface == "" {
		return nil
	}
	return setBindToDevice(c, iface)
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
// to dst by parsing /proc/net/route. It excludes the TUN interface and falls
// back to defaultIface if no route matches.
func linuxRouteIface(dst net.IP, tunIface, defaultIface string) (string, error) {
	if dst == nil {
		return defaultIface, nil
	}
	dst4 := dst.To4()
	if dst4 == nil {
		return defaultIface, nil
	}
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return "", err
	}
	defer f.Close()

	dstU := ipToUint32LE(dst4)
	var bestIface string
	bestPrefix := -1

	scanner := bufio.NewScanner(f)
	first := true
	for scanner.Scan() {
		if first {
			first = false
			continue
		}
		fields := strings.Fields(scanner.Text())
		if len(fields) < 8 {
			continue
		}
		iface := strings.TrimSpace(fields[0])
		if iface == tunIface {
			continue
		}
		destU, err1 := strconv.ParseUint(fields[1], 16, 32)
		maskU, err2 := strconv.ParseUint(fields[7], 16, 32)
		if err1 != nil || err2 != nil {
			continue
		}
		mask := uint32(maskU)
		dest := uint32(destU)
		if dstU&mask != dest&mask {
			continue
		}
		prefix := bits.OnesCount32(mask)
		if prefix > bestPrefix {
			bestPrefix = prefix
			bestIface = iface
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if bestIface == "" {
		return defaultIface, nil
	}
	return bestIface, nil
}

// ipToUint32LE converts an IPv4 address to a uint32 in the same byte order as
// /proc/net/route (host / little-endian).
func ipToUint32LE(ip net.IP) uint32 {
	ip = ip.To4()
	return uint32(ip[0]) | uint32(ip[1])<<8 | uint32(ip[2])<<16 | uint32(ip[3])<<24
}
