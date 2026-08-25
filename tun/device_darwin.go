//go:build darwin

package tun

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

type darwinTUN struct {
	fd   int
	name string
	mtu  int
	rBuf []byte // pre-allocated read buffer to avoid per-packet allocation
}

// CreateDevice creates a macOS utun device.
func CreateDevice() (Device, error) {
	fd, err := unix.Socket(unix.AF_SYSTEM, unix.SOCK_DGRAM, 2)
	if err != nil {
		return nil, fmt.Errorf("create utun socket: %w", err)
	}

	var ctlInfo unix.CtlInfo
	copy(ctlInfo.Name[:], "com.apple.net.utun_control")

	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(fd),
		uintptr(unix.CTLIOCGINFO),
		uintptr(unsafe.Pointer(&ctlInfo)),
	)
	if errno != 0 {
		unix.Close(fd)
		return nil, fmt.Errorf("ctlciocginfo: %w", errno)
	}

	addr := unix.SockaddrCtl{
		ID:   ctlInfo.Id,
		Unit: 0,
	}

	if err := unix.Connect(fd, &addr); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("connect utun: %w", err)
	}

	const (
		syspControl   = 2
		utunOptIfname = 2
	)
	var ifName [unix.IFNAMSIZ]byte
	ifNameLen := unix.IFNAMSIZ
	_, _, errno = unix.Syscall6(
		unix.SYS_GETSOCKOPT,
		uintptr(fd),
		syspControl,
		utunOptIfname,
		uintptr(unsafe.Pointer(&ifName)),
		uintptr(unsafe.Pointer(&ifNameLen)),
		0,
	)
	if errno != 0 {
		unix.Close(fd)
		return nil, fmt.Errorf("get utun ifname: %w", errno)
	}

	name := string(ifName[:ifNameLen-1])
	unix.SetNonblock(fd, true)

	return &darwinTUN{
		fd:   fd,
		name: name,
		mtu:  1500,
		rBuf: make([]byte, 65535),
	}, nil
}

func (t *darwinTUN) Name() string { return t.name }
func (t *darwinTUN) GUID() string { return "" }
func (t *darwinTUN) MTU() int     { return t.mtu }
func (t *darwinTUN) Close() error { return unix.Close(t.fd) }

func (t *darwinTUN) Read(buf []byte) (int, error) {
	n, err := unix.Read(t.fd, t.rBuf)
	if err != nil {
		return 0, err
	}
	if n <= 4 {
		return 0, nil
	}
	return copy(buf, t.rBuf[4:n]), nil
}

func (t *darwinTUN) Write(buf []byte) (int, error) {
	hdr := [4]byte{0, 0, 0, 2}
	_, err := unix.Write(t.fd, append(hdr[:], buf...))
	if err != nil {
		return 0, err
	}
	return len(buf), nil
}
