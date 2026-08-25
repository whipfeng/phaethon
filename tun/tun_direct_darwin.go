//go:build darwin

package tun

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// setDirectSocketOption binds the socket to the original physical interface
// so that packets bypass the TUN split-tunnel routes. On macOS this is done
// via IP_BOUND_IF (IPv4) and IPV6_BOUND_IF (IPv6). Both are set so the socket
// works regardless of which address family the connection uses.
func setDirectSocketOption(c syscall.RawConn, ifaceName string, ifaceIndex int) error {
	var sockErr error
	err := c.Control(func(fd uintptr) {
		if e := unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_BOUND_IF, ifaceIndex); e != nil {
			sockErr = e
			return
		}
		_ = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_BOUND_IF, ifaceIndex)
	})
	if err != nil {
		return err
	}
	return sockErr
}
