//go:build linux

package tun

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// setDirectSocketOption binds the socket to the original physical interface
// so that packets bypass the TUN split-tunnel routes and use the original
// default route instead. On Linux this is done via SO_BINDTODEVICE.
func setDirectSocketOption(c syscall.RawConn, ifaceName string, ifaceIndex int) error {
	var sockErr error
	err := c.Control(func(fd uintptr) {
		sockErr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, ifaceName)
	})
	if err != nil {
		return err
	}
	return sockErr
}
