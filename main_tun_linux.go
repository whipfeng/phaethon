//go:build linux

package main

import (
	"syscall"
	"unsafe"
)

// setProcessName sets the process name visible in ps/top via prctl(PR_SET_NAME).
// The name is truncated to 15 bytes by the kernel.
func setProcessName(name string) {
	nameBytes, _ := syscall.ByteSliceFromString(name)
	_, _, _ = syscall.Syscall(syscall.SYS_PRCTL, 15, uintptr(unsafe.Pointer(&nameBytes[0])), 0)
}
