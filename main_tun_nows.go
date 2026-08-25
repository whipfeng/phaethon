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
	exe, err := os.Executable()
	if err != nil {
		util.LogWarn("tun: cannot get executable path for watchdog: %v", err)
		return
	}
	pid := os.Getpid()
	env := append(os.Environ(), "LAYER_WATCHDOG_PID="+strconv.Itoa(pid))
	attr := &os.ProcAttr{
		Env:   env,
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
	}
	p, err := os.StartProcess(exe, []string{exe}, attr)
	if err != nil {
		util.LogWarn("tun: failed to spawn watchdog: %v", err)
		return
	}
	util.LogDebug("tun: watchdog spawned (pid=%d)", p.Pid)
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
