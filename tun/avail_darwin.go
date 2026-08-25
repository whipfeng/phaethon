//go:build darwin

package tun

import (
	"unsafe"

	"golang.org/x/sys/unix"
	"phaethon/util"
)

// Available reports whether TUN can be initialized on macOS.
// It performs a lightweight runtime check by attempting to connect to the
// utun control socket.
func Available() bool {
	fd, err := unix.Socket(unix.AF_SYSTEM, unix.SOCK_DGRAM, 2)
	if err != nil {
		util.LogWarn("tun: unable to create system control socket: %v", err)
		return false
	}
	defer unix.Close(fd)

	var ctlInfo unix.CtlInfo
	copy(ctlInfo.Name[:], "com.apple.net.utun_control")

	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(fd),
		uintptr(unix.CTLIOCGINFO),
		uintptr(unsafe.Pointer(&ctlInfo)),
	)
	if errno != 0 {
		util.LogWarn("tun: utun control not available: %v", errno)
		return false
	}
	return true
}
