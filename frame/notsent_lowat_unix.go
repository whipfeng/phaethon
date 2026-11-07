//go:build linux || darwin

package frame

import (
	"net"

	"golang.org/x/sys/unix"
)

func setNotSentLowat(tc *net.TCPConn, n int) error {
	rc, err := tc.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := rc.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT, n)
	}); err != nil {
		return err
	}
	return serr
}
