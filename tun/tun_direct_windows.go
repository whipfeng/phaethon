//go:build windows

package tun

import (
	"syscall"
)

// setDirectSocketOption binds the socket to the original physical interface
// so that packets bypass the TUN split-tunnel routes. On Windows this is done
// via IP_UNICAST_IF (option value 31 at IPPROTO_IP level).
func setDirectSocketOption(c syscall.RawConn, ifaceName string, ifaceIndex int) error {
	var sockErr error
	err := c.Control(func(fd uintptr) {
		sockErr = syscall.SetsockoptInt(syscall.Handle(fd), syscall.IPPROTO_IP, 31, ifaceIndex)
	})
	if err != nil {
		return err
	}
	return sockErr
}
