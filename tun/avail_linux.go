//go:build linux

package tun

import (
	"golang.org/x/sys/unix"
	"phaethon/util"
)

// Available reports whether TUN can be initialized on Linux.
// It performs a lightweight runtime check by attempting to open /dev/net/tun.
// If the device doesn't exist, it attempts to create it automatically.
func Available() bool {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR, 0)
	if err != nil {
		// If device doesn't exist, try to create it
		if err == unix.ENOENT {
			util.LogModuleInfo("tun", "/dev/net/tun not found, attempting to create")
			if createErr := createTUNDevice(); createErr != nil {
				util.LogModuleWarn("tun", "failed to create /dev/net/tun: %v", createErr)
				return false
			}
			// Try opening again
			fd, err = unix.Open("/dev/net/tun", unix.O_RDWR, 0)
			if err != nil {
				util.LogModuleWarn("tun", "/dev/net/tun not available after creation: %v", err)
				return false
			}
		} else {
			util.LogModuleWarn("tun", "/dev/net/tun not available: %v", err)
			return false
		}
	}
	_ = unix.Close(fd)
	return true
}

// createTUNDevice creates the /dev/net/tun character device.
func createTUNDevice() error {
	// Create /dev/net directory if it doesn't exist
	if err := unix.Mkdir("/dev/net", 0755); err != nil && err != unix.EEXIST {
		return err
	}
	// Create character device: major 10, minor 200
	dev := unix.Mkdev(10, 200)
	if err := unix.Mknod("/dev/net/tun", unix.S_IFCHR|0600, int(dev)); err != nil {
		return err
	}
	util.LogModuleInfo("tun", "created /dev/net/tun device")
	return nil
}
