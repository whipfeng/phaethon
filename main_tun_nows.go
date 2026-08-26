//go:build !windows

package main

import (
	"os"
	"strconv"
	"syscall"

	"phaethon/util"
)

// spawnWatchdog starts a child process that monitors this process lifetime.
func spawnWatchdog() {
	wdExe, err := ensureWatchdogExecutable()
	if err != nil {
		util.LogWarn("tun: cannot prepare watchdog executable: %v", err)
		return
	}
	pid := os.Getpid()
	env := append(os.Environ(), "LAYER_WATCHDOG_PID="+strconv.Itoa(pid))
	attr := &os.ProcAttr{
		Env:   env,
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
	}
	p, err := os.StartProcess(wdExe, []string{wdExe}, attr)
	if err != nil {
		util.LogWarn("tun: failed to spawn watchdog: %v", err)
		return
	}
	util.LogInfo("tun: watchdog spawned (pid=%d) from %s", p.Pid, wdExe)
}

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
