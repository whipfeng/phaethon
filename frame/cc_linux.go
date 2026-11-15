//go:build linux

package frame

import (
	"net"

	"golang.org/x/sys/unix"
)

// setCongestionControl selects the kernel congestion-control algorithm for one
// carrier socket (per-socket, no sysctl and no global default change). Used by
// the isolated BBR reference experiment; inert unless PHAETHON_CARRIER_CC is
// set on the sender process.
func setCongestionControl(tc *net.TCPConn, cc string) error {
	rc, err := tc.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := rc.Control(func(fd uintptr) {
		serr = unix.SetsockoptString(int(fd), unix.IPPROTO_TCP, unix.TCP_CONGESTION, cc)
	}); err != nil {
		return err
	}
	return serr
}
