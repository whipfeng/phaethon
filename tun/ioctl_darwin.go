//go:build darwin

package tun

import (
	"fmt"
	"net"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Structured interface configuration via ioctl — replaces the ifconfig(8)
// exec calls in route_darwin.go / cleanup_darwin.go. Layouts mirror xnu:
//
//	struct ifaliasreq { char ifra_name[IFNAMSIZ=16]; struct sockaddr ifra_addr;
//	                    struct sockaddr ifra_dstaddr; struct sockaddr ifra_mask; }
//	struct ifreq       { char ifr_name[16]; union { struct sockaddr ifru_addr;
//	                    short ifru_flags; ... } ifr_ifru; }   // sizeof == 32
//
// sockaddr_in is always the full 16-byte form on input (the kernel does not
// accept the trimmed form it emits in RTM_GET replies).

type ifAliasreq struct {
	name [unix.IFNAMSIZ]byte
	addr [16]byte
	dst  [16]byte
	mask [16]byte
}

var _ [unsafe.Sizeof(ifAliasreq{})]byte = [16 + 48]byte{}

type ifreqShort struct {
	name  [unix.IFNAMSIZ]byte
	flags uint16
	pad   [14]byte
}

var _ [unsafe.Sizeof(ifreqShort{})]byte = [32]byte{}

// ifreqAddr is struct ifreq with the ifru_addr union member (for SIOCDIFADDR).
type ifreqAddr struct {
	name [unix.IFNAMSIZ]byte
	addr [16]byte
}

var _ [unsafe.Sizeof(ifreqAddr{})]byte = [32]byte{}

func sockaddrIn16(ip net.IP) [16]byte {
	var b [16]byte
	b[0] = 16
	b[1] = unix.AF_INET
	copy(b[4:8], ip.To4())
	return b
}

func ifaceCtlSocket() (int, error) {
	return unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
}

func ifnameBytes(name string) [unix.IFNAMSIZ]byte {
	var b [unix.IFNAMSIZ]byte
	copy(b[:], name)
	return b
}

// setIfaceAddr configures ip/mask on the named interface (SIOCAIFADDR), the
// ioctl equivalent of `ifconfig <dev> inet <ip> <ip> netmask <mask>`. The
// destination address equals the interface address, matching ifconfig's
// behavior on the point-to-point utun.
func setIfaceAddr(name string, ip net.IP, prefixLen int) error {
	fd, err := ifaceCtlSocket()
	if err != nil {
		return err
	}
	defer unix.Close(fd)

	ip4 := ip.To4()
	if ip4 == nil {
		return fmt.Errorf("setIfaceAddr: %v is not IPv4", ip)
	}
	var req ifAliasreq
	req.name = ifnameBytes(name)
	req.addr = sockaddrIn16(ip4)
	req.dst = sockaddrIn16(ip4)
	req.mask = sockaddrIn16(net.IP(net.CIDRMask(prefixLen, 32)))

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.SIOCAIFADDR), uintptr(unsafe.Pointer(&req)))
	if errno != 0 {
		return fmt.Errorf("SIOCAIFADDR %s %s/%d: %w", name, ip4, prefixLen, errno)
	}
	return nil
}

// setIfaceUp brings the named interface up or down (SIOCGIFFLAGS/SIOCSIFFLAGS),
// the ioctl equivalent of `ifconfig <dev> up|down`.
func setIfaceUp(name string, up bool) error {
	fd, err := ifaceCtlSocket()
	if err != nil {
		return err
	}
	defer unix.Close(fd)

	var req ifreqShort
	req.name = ifnameBytes(name)
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.SIOCGIFFLAGS), uintptr(unsafe.Pointer(&req)))
	if errno != 0 {
		return fmt.Errorf("SIOCGIFFLAGS %s: %w", name, errno)
	}
	if up {
		req.flags |= unix.IFF_UP
	} else {
		req.flags &^= unix.IFF_UP
	}
	_, _, errno = unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.SIOCSIFFLAGS), uintptr(unsafe.Pointer(&req)))
	if errno != 0 {
		return fmt.Errorf("SIOCSIFFLAGS %s: %w", name, errno)
	}
	return nil
}

// delIfaceAddr removes ip from the named interface (SIOCDIFADDR), the ioctl
// equivalent of `ifconfig <dev> delete <ip>`. The kernel matches the address
// record by IP alone, so no mask is passed — same semantics as ifconfig.
func delIfaceAddr(name string, ip net.IP) error {
	fd, err := ifaceCtlSocket()
	if err != nil {
		return err
	}
	defer unix.Close(fd)

	ip4 := ip.To4()
	if ip4 == nil {
		return fmt.Errorf("delIfaceAddr: %v is not IPv4", ip)
	}
	var req ifreqAddr
	req.name = ifnameBytes(name)
	req.addr = sockaddrIn16(ip4)

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.SIOCDIFADDR), uintptr(unsafe.Pointer(&req)))
	if errno != 0 {
		return fmt.Errorf("SIOCDIFADDR %s %s: %w", name, ip4, errno)
	}
	return nil
}
