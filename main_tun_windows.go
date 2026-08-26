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
		Sys:   &syscall.SysProcAttr{CreationFlags: 0x08000000},
	}
	p, err := os.StartProcess(wdExe, []string{wdExe}, attr)
	if err != nil {
		util.LogWarn("tun: failed to spawn watchdog: %v", err)
		return
	}
	util.LogInfo("tun: watchdog spawned (pid=%d) from %s", p.Pid, wdExe)
}

var consoleCloseCh = make(chan struct{}, 1)

func init() {
	k32 := syscall.NewLazyDLL("kernel32.dll")
	proc := k32.NewProc("SetConsoleCtrlHandler")
	cb := syscall.NewCallback(func(ctrlType uint32) uintptr {
		if ctrlType == 2 { // CTRL_CLOSE_EVENT
			select {
			case consoleCloseCh <- struct{}{}:
			default:
			}
			return 1
		}
		return 0
	})
	proc.Call(cb, 1)
}

// consoleCloseNotify returns a channel that receives a value when the console
// window is closed (WM_CLOSE / CTRL_CLOSE_EVENT).
func consoleCloseNotify() <-chan struct{} {
	return consoleCloseCh
}

// processExists checks whether a process with the given PID is still running.
// OpenProcess alone can succeed for a terminated zombie, so we also check the
// exit code via GetExitCodeProcess.
func processExists(pid int) bool {
	handle, err := syscall.OpenProcess(syscall.PROCESS_QUERY_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(handle)
	var exitCode uint32
	if err := syscall.GetExitCodeProcess(handle, &exitCode); err != nil {
		return false
	}
	return exitCode == 259
}
