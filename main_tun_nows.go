//go:build !windows

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"phaethon/util"
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
func reapChild(pid int) (exitCode int, ok bool) {
	var ws syscall.WaitStatus
	_, err := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
	if err != nil {
		return 0, false
	}
	if ws.Exited() {
		return ws.ExitStatus(), true
	}
	return 0, false
}

// waitForProcessExit waits for a process to exit by polling.
func waitForProcessExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processExists(pid) {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return !processExists(pid)
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

// killResidualWorkers finds and kills any orphaned phaethon processes — both
// worker processes (PHAETHON_WORKER=1) and leftover watchdog/parent processes
// from a previous run. Must be called BEFORE spawning a new child so the new
// child is never mistaken for a residual.
func killResidualWorkers() {
	if _, err := os.Stat("/proc"); err != nil {
		return
	}
	myPid := os.Getpid()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	var residualPids []int
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == myPid {
			continue
		}
		cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			continue
		}
		if !strings.Contains(string(cmdline), "phaethon") {
			continue
		}
		// Skip supervise-daemon (it has "supervise-daemon" in cmdline, not "phaethon" as the binary)
		if strings.Contains(string(cmdline), "supervise-daemon") {
			continue
		}
		residualPids = append(residualPids, pid)
	}
	if len(residualPids) == 0 {
		return
	}
	util.LogInfo("watchdog: found %d residual process(es): %v", len(residualPids), residualPids)
	for _, pid := range residualPids {
		p, err := os.FindProcess(pid)
		if err == nil {
			_ = p.Signal(syscall.SIGTERM)
		}
	}
	time.Sleep(3 * time.Second)
	for _, pid := range residualPids {
		if processExists(pid) {
			p, err := os.FindProcess(pid)
			if err == nil {
				_ = p.Signal(syscall.SIGKILL)
			}
		}
	}
	time.Sleep(1 * time.Second)
	for _, pid := range residualPids {
		reapChild(pid)
	}
	util.LogInfo("watchdog: killed %d residual process(es)", len(residualPids))
}
