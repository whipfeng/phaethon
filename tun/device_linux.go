//go:build linux

package tun

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

type linuxTUN struct {
	fd   int
	name string
	mtu  int
}

// CreateDevice creates a Linux TUN device.
func CreateDevice() (Device, error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/net/tun: %w", err)
	}

	var ifr struct {
		name  [unix.IFNAMSIZ]byte
		flags uint16
		_     [20]byte
	}
	copy(ifr.name[:], "tun0")
	ifr.flags = unix.IFF_TUN | unix.IFF_NO_PI

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.TUNSETIFF, uintptr(unsafe.Pointer(&ifr)))
	if errno != 0 {
		unix.Close(fd)
		return nil, fmt.Errorf("tunsetiff: %w", errno)
	}

	tun := &linuxTUN{
		fd:   fd,
		name: string(ifr.name[:]),
		mtu:  1500,
	}

	unix.SetNonblock(fd, true)
	return tun, nil
}

func (t *linuxTUN) Name() string { return t.name }
func (t *linuxTUN) GUID() string { return "" }
func (t *linuxTUN) MTU() int     { return t.mtu }
func (t *linuxTUN) Close() error { return unix.Close(t.fd) }

func (t *linuxTUN) Read(buf []byte) (int, error) {
	n, err := unix.Read(t.fd, buf)
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (t *linuxTUN) Write(buf []byte) (int, error) {
	return unix.Write(t.fd, buf)
}
