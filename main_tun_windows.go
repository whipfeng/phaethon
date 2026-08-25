//go:build windows

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
		Sys:   &syscall.SysProcAttr{CreationFlags: 0x08000000},
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
	handle, err := syscall.OpenProcess(syscall.PROCESS_QUERY_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	syscall.CloseHandle(handle)
	return true
}
