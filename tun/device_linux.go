//go:build linux

package tun

import (
	"fmt"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// pollTimeoutMs bounds how long a read/write waits for the device to become
// ready. It is also the upper bound for how long Close can be delayed by an
// in-flight Read/Write, so StopTUN's goroutine wait cannot hang on an idle TUN.
const pollTimeoutMs = 200

type linuxTUN struct {
	// mu guards fd/closed. Every read/write syscall holds RLock so Close can
	// never release a fd number while a syscall is about to use it (a reused
	// fd number would otherwise point at an unrelated socket).
	mu     sync.RWMutex
	fd     int
	name   string
	mtu    int
	closed bool
}

// CreateDevice creates a Linux TUN device.
func CreateDevice() (Device, error) {
	// Non-blocking: a blocking read cannot be interrupted by Close (the
	// in-flight read holds a reference to the file), so StopTUN used to wait
	// forever for readLoop on an idle device. With O_NONBLOCK the loops poll
	// with a timeout and observe the close flag; traffic still costs exactly
	// one read syscall per packet.
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/net/tun: %w", err)
	}
	// Never let child processes (built-in SSH shells, watchers, exec'd
	// workers) inherit the TUN fd. An inherited fd keeps the interface
	// alive after this worker restarts, so the next instance gets tunN+1
	// while the stale tunN keeps the same /29 route — the kernel can then
	// route the DNS GIP to the dead interface (observed on QG).
	unix.CloseOnExec(fd)

	var ifr struct {
		name  [unix.IFNAMSIZ]byte
		flags uint16
		_     [20]byte
	}
	// Ask the kernel for the first free tunN instead of pinning tun0. The
	// kernel writes the assigned name back into ifr.name; every consumer below
	// uses dev.Name(), so multiple instances no longer collide on one name.
	copy(ifr.name[:], "tun%d")
	ifr.flags = unix.IFF_TUN | unix.IFF_NO_PI

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.TUNSETIFF, uintptr(unsafe.Pointer(&ifr)))
	if errno != 0 {
		unix.Close(fd)
		return nil, fmt.Errorf("tunsetiff: %w (%s)", errno, describeTUNConflict())
	}

	tun := &linuxTUN{
		fd:   fd,
		name: strings.TrimRight(string(ifr.name[:]), "\x00"),
		mtu:  OverlayMTU,
	}

	return tun, nil
}

func (t *linuxTUN) Name() string { return t.name }
func (t *linuxTUN) GUID() string { return "" }
func (t *linuxTUN) MTU() int     { return t.mtu }

func (t *linuxTUN) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	t.closed = true
	return unix.Close(t.fd)
}

func (t *linuxTUN) Read(buf []byte) (int, error) {
	for {
		t.mu.RLock()
		if t.closed {
			t.mu.RUnlock()
			return 0, ErrSessionClosed
		}
		n, err := unix.Read(t.fd, buf)
		if err == nil {
			t.mu.RUnlock()
			return n, nil
		}
		if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
			// Wait for a packet, but wake up periodically so Close (operator
			// toggle / shutdown) is observed even when no traffic arrives.
			_, pollErr := unix.Poll([]unix.PollFd{{Fd: int32(t.fd), Events: unix.POLLIN}}, pollTimeoutMs)
			t.mu.RUnlock()
			if pollErr == unix.EINTR {
				continue
			}
			if pollErr != nil {
				return 0, pollErr
			}
			continue
		}
		t.mu.RUnlock()
		if err == unix.EINTR {
			continue
		}
		// The device was detached underneath us (ip link del / driver reset):
		// reads of a dead tun fd return EIO/ENXIO. Report session death so the
		// read loop exits and the health loop recreates the interface.
		if err == unix.EIO || err == unix.ENXIO || err == unix.EBADF {
			return 0, ErrSessionClosed
		}
		return 0, err
	}
}

func (t *linuxTUN) Write(buf []byte) (int, error) {
	for {
		t.mu.RLock()
		if t.closed {
			t.mu.RUnlock()
			return 0, ErrSessionClosed
		}
		n, err := unix.Write(t.fd, buf)
		if err == nil {
			t.mu.RUnlock()
			return n, nil
		}
		if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
			// TX queue full: wait for space instead of dropping the packet.
			_, pollErr := unix.Poll([]unix.PollFd{{Fd: int32(t.fd), Events: unix.POLLOUT}}, pollTimeoutMs)
			t.mu.RUnlock()
			if pollErr == unix.EINTR {
				continue
			}
			if pollErr != nil {
				return 0, pollErr
			}
			continue
		}
		t.mu.RUnlock()
		if err == unix.EINTR {
			continue
		}
		if err == unix.EIO || err == unix.ENXIO || err == unix.EBADF {
			return 0, ErrSessionClosed
		}
		return 0, err
	}
}
