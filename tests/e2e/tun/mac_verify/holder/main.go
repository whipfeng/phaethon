//go:build darwin

// Holder for the 198.18 coexistence test: occupies a scratch utun with a
// 198.18.x.x address plus 198.18/16 and 0/1 routes, then parks so phaethon's
// CleanupResidual can be observed against it. Only test prefixes are touched.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func main() {
	if os.Geteuid() != 0 {
		fmt.Println("FATAL: must run as root")
		os.Exit(1)
	}
	fd, name := createUTUN()
	defer unix.Close(fd)

	must("ifconfig", name, "inet", "198.18.0.1", "198.18.0.1", "netmask", "255.255.0.0", "up")
	must("route", "add", "-net", "198.18.0.0/16", "198.18.0.1")
	must("route", "add", "-net", "0/1", "198.18.0.1")

	fmt.Printf("HOLDER-READY iface=%s ip=198.18.0.1\n", name)
	for {
		time.Sleep(time.Hour)
	}
}

func must(name string, args ...string) {
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		fmt.Printf("FATAL %s %v: %v %s\n", name, args, err, out)
		os.Exit(1)
	}
}

func createUTUN() (int, string) {
	fd, err := unix.Socket(unix.AF_SYSTEM, unix.SOCK_DGRAM, 2)
	if err != nil {
		fmt.Println("FATAL socket:", err)
		os.Exit(1)
	}
	var ctlInfo unix.CtlInfo
	copy(ctlInfo.Name[:], "com.apple.net.utun_control")
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.CTLIOCGINFO), uintptr(unsafe.Pointer(&ctlInfo)))
	if errno != 0 {
		fmt.Println("FATAL CTLIOCGINFO:", errno)
		os.Exit(1)
	}
	addr := unix.SockaddrCtl{ID: ctlInfo.Id, Unit: 0}
	if err := unix.Connect(fd, &addr); err != nil {
		fmt.Println("FATAL connect:", err)
		os.Exit(1)
	}
	const utunOptIfname = 2
	var ifName [unix.IFNAMSIZ]byte
	ifNameLen := unix.IFNAMSIZ
	_, _, errno = unix.Syscall6(unix.SYS_GETSOCKOPT, uintptr(fd), 2, utunOptIfname, uintptr(unsafe.Pointer(&ifName)), uintptr(unsafe.Pointer(&ifNameLen)), 0)
	if errno != 0 {
		fmt.Println("FATAL getsockopt:", errno)
		os.Exit(1)
	}
	return fd, string(ifName[:ifNameLen-1])
}
