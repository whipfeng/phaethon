//go:build linux || darwin

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// takeoverInstanceLock terminates the live instance that owns dataDir and waits
// for its lock to be released. Only an explicit --takeover calls this: the
// default startup path never kills anything.
//
// Order matters: the watchdog (parent) is signalled first so it cannot respawn
// a replacement worker that would race us for the lock. On Linux the worker
// also receives SIGTERM through PR_SET_PDEATHSIG.
func takeoverInstanceLock(dataDir string) error {
	path := filepath.Join(dataDir, "instance.lock")
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("read lock file: %w", err)
	}
	holder, ok := readLockHolder(f)
	f.Close()
	if !ok {
		return fmt.Errorf("lock is held but its owner cannot be identified; stop the other instance manually")
	}
	if !sameDeployment(holder.PID, holder.Exe) {
		return fmt.Errorf("lock owner pid %d is not this deployment's phaethon (%s); refusing to kill it", holder.PID, holder.Exe)
	}

	targets := []int{holder.PID}
	if ppid := readProcessParent(holder.PID); ppid > 1 && sameDeployment(ppid, holder.Exe) {
		targets = []int{ppid, holder.PID}
	}
	for _, pid := range targets {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Signal(syscall.SIGTERM)
		}
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		f, err := os.OpenFile(path, os.O_RDWR, 0644)
		if err == nil {
			err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
			if err == nil {
				_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
				f.Close()
				return nil
			}
			f.Close()
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for pid %d to release %s", holder.PID, path)
}

// sameDeployment reports whether pid runs this phaethon binary (same path, or
// same directory + file name). On platforms without /proc the executable path
// cannot be read, so the path recorded in the lock file is used instead — the
// lock file lives in the data directory and is only written by its owner.
func sameDeployment(pid int, lockHolderExe string) bool {
	exe := readProcessExe(pid)
	if exe == "" {
		exe = lockHolderExe
	}
	if pid <= 1 || exe == "" {
		return false
	}
	self, err := os.Executable()
	if err != nil {
		return false
	}
	if exe == self {
		return true
	}
	return filepath.Base(exe) == filepath.Base(self) && filepath.Dir(exe) == filepath.Dir(self)
}
