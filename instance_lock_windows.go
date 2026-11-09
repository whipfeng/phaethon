//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// acquireInstanceLock takes an exclusive lock on <dataDir>/instance.lock using
// LockFileEx. Only the worker holds it; the watchdog never does.
//
// A successful lock means "no other instance is alive for this data
// directory", which is what makes startup residual cleanup safe: any leftover
// Wintun adapter, route or DNS change must belong to a previous incarnation of
// this same instance. A failed lock means another instance owns the directory,
// so this process exits instead of deleting anything.
func acquireInstanceLock(dataDir string) (func(), error) {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, "instance.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	ol := new(windows.Overlapped)
	err = windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if err != nil {
		holder, _ := readLockHolder(f)
		f.Close()
		return nil, fmt.Errorf("another instance holds %s (%s): %w", path, holder.describe(), err)
	}
	writeLockHolder(f)
	return func() {
		_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ol)
		f.Close()
	}, nil
}
