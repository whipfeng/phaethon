//go:build linux || darwin

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// acquireInstanceLock takes an exclusive, non-blocking flock on
// <dataDir>/instance.lock. Only the worker holds it; the watchdog never does.
//
// A successful lock means "no other instance is alive for this data
// directory", which is what makes startup residual cleanup safe: any leftover
// TUN device, route or DNS change must belong to a previous incarnation of
// this same instance. A failed lock means another instance owns the
// directory, so this process exits instead of killing anything.
func acquireInstanceLock(dataDir string) (func(), error) {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, "instance.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		holder, _ := readLockHolder(f)
		f.Close()
		return nil, fmt.Errorf("another instance holds %s (%s): %w", path, holder.describe(), err)
	}
	writeLockHolder(f)
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	}, nil
}
