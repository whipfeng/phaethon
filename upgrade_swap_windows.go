//go:build windows

package main

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"phaethon/util"
)

var replaceFileWProc = syscall.NewLazyDLL("kernel32.dll").NewProc("ReplaceFileW")

// installBinary replaces exe with newPath on Windows. ReplaceFileW performs
// the replacement and backup creation in one call, so the stable path is never
// absent. If it is unavailable/fails, fall back to the historical rename-away
// sequence (which has a tiny missing-path window; the deployment launcher is
// expected to repair that case).
func installBinary(exe, newPath, backupPath string) error {
	if ok, code := replaceFileW(exe, newPath, backupPath); ok {
		return nil
	} else {
		util.LogModuleWarn("main", "watchdog: ReplaceFileW failed (error=%d), falling back to rename", code)
	}

	if err := os.Rename(exe, backupPath); err != nil {
		return fmt.Errorf("move current binary aside: %w", err)
	}
	if err := os.Rename(newPath, exe); err != nil {
		_ = os.Rename(backupPath, exe)
		return fmt.Errorf("install staged binary: %w", err)
	}
	return nil
}

func replaceFileW(replaced, replacement, backup string) (bool, uint32) {
	rp, err := syscall.UTF16PtrFromString(replaced)
	if err != nil {
		return false, uint32(syscall.EINVAL)
	}
	np, err := syscall.UTF16PtrFromString(replacement)
	if err != nil {
		return false, uint32(syscall.EINVAL)
	}
	var bp *uint16
	if backup != "" {
		bp, err = syscall.UTF16PtrFromString(backup)
		if err != nil {
			return false, uint32(syscall.EINVAL)
		}
	}
	r, _, e := replaceFileWProc.Call(
		uintptr(unsafe.Pointer(rp)),
		uintptr(unsafe.Pointer(np)),
		uintptr(unsafe.Pointer(bp)),
		0, 0, 0,
	)
	if r != 0 {
		return true, 0
	}
	if errno, ok := e.(syscall.Errno); ok {
		return false, uint32(errno)
	}
	return false, 1
}

// syncParentDir is a no-op on Windows; ReplaceFileW handles the replacement
// atomically and directory handles are not fsync-able portably.
func syncParentDir(path string) {}
