//go:build linux

package frame

import (
	"net"
	"unsafe"

	"golang.org/x/sys/unix"
)

// setMaxPacingRate caps the kernel pacing rate of one carrier socket. Unlike
// TCP_NOTSENT_LOWAT (unsent bytes only) this bounds the rate at which the
// kernel pushes segments into the path, so a slow-start burst cannot overflow
// a shallow WAN buffer. Per-socket, no sysctl and no global effect.
func setMaxPacingRate(tc *net.TCPConn, bytesPerSec uint64) error {
	rc, err := tc.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := rc.Control(func(fd uintptr) {
		v := bytesPerSec
		_, _, e := unix.Syscall6(unix.SYS_SETSOCKOPT, fd, unix.SOL_SOCKET, unix.SO_MAX_PACING_RATE,
			uintptr(unsafe.Pointer(&v)), unsafe.Sizeof(v), 0)
		if e != 0 {
			serr = e
		}
	}); err != nil {
		return err
	}
	return serr
}
