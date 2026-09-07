//go:build !windows

package main

import (
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// consoleCloseNotify returns a channel that is never closed on non-Windows
// platforms. Console close events are handled via normal signals there.
func consoleCloseNotify() <-chan struct{} {
	return make(chan struct{})
}

// processExists checks whether a process with the given PID is still running.
func processExists(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil
}

// reapChild reaps a zombie child process. The watchdog becomes the parent of
// the server process, so it must wait on it to avoid zombies.
func reapChild(pid int) {
	var ws syscall.WaitStatus
	_, _ = syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
}

// reexecAsWatchdog copies the current binary to a "-watchdog" suffixed name
// and re-execs into it via syscall.Exec. The PID stays the same so init
// systems (OpenRC, systemd) continue tracking the process normally.
// If already running as the watchdog copy, returns immediately.
func reexecAsWatchdog() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	dir := filepath.Dir(exe)
	ext := filepath.Ext(exe)
	base := filepath.Base(exe)
	if len(ext) > 0 && len(base) > len(ext) {
		base = base[:len(base)-len(ext)]
	}
	if base == "phaethon-watchdog" {
		return
	}
	wdPath := filepath.Join(dir, "phaethon-watchdog"+ext)
	if err := copyFile(exe, wdPath); err != nil {
		if _, statErr := os.Stat(wdPath); statErr == nil {
			// Already exists (possibly locked), reuse it.
		} else {
			return
		}
	}
	_ = syscall.Exec(wdPath, []string{wdPath}, os.Environ())
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}
