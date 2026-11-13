//go:build !windows

package main

import (
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
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

// reapChild reaps a zombie child process and returns its exit code.
// The watchdog becomes the parent of the server process, so it must
// wait on it to avoid zombies.
//
// The stdout pipe can close a moment before the kernel marks the child
// reapable, so a single non-blocking Wait4 used to leave zombies behind.
// Poll briefly instead of giving up on the first EAGAIN/0 result.
func reapChild(pid int) (exitCode int, ok bool) {
	deadline := time.Now().Add(2 * time.Second)
	for {
		var ws syscall.WaitStatus
		wpid, err := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
		if err == nil && wpid == pid {
			if ws.Exited() {
				return ws.ExitStatus(), true
			}
			if ws.Signaled() {
				return 128 + int(ws.Signal()), true
			}
			return 0, true
		}
		if err != nil {
			// ECHILD: already reaped by someone else.
			return 0, false
		}
		if time.Now().After(deadline) {
			return 0, false
		}
		time.Sleep(50 * time.Millisecond)
	}
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
