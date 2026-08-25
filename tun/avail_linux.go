//go:build linux

package tun

import (
	"golang.org/x/sys/unix"
	"phaethon/util"
)

// Available reports whether TUN can be initialized on Linux.
// It performs a lightweight runtime check by attempting to open /dev/net/tun.
func Available() bool {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR, 0)
	if err != nil {
		util.LogWarn("tun: /dev/net/tun not available: %v", err)
		return false
	}
	_ = unix.Close(fd)
	return true
}
