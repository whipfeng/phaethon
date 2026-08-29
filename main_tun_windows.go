//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"

	"phaethon/util"
)

// spawnWatchdog starts a detached child process that monitors this process
// lifetime. It logs to its own file and is not attached to the parent console,
// so it can clean up even if the parent console is closed or the parent hangs.
func spawnWatchdog(probeURLs []string) {
	wdExe, err := ensureWatchdogExecutable()
	if err != nil {
		util.LogWarn("tun: cannot prepare watchdog executable: %v", err)
		return
	}

	logPath := filepath.Join(filepath.Dir(wdExe), "phaethon-watchdog.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		util.LogWarn("tun: cannot open watchdog log %s: %v", logPath, err)
		return
	}

	pid := os.Getpid()
	env := append(os.Environ(), "LAYER_WATCHDOG_PID="+strconv.Itoa(pid))
	if probeURLs != nil {
		env = append(env, "LAYER_WATCHDOG_PROBE_URLS="+strings.Join(probeURLs, ";"))
	}
	attr := &os.ProcAttr{
		Env:   env,
		Files: []*os.File{nil, logFile, logFile},
		Sys: &syscall.SysProcAttr{
			// DETACHED_PROCESS: no console, so closing the parent console will
			// not kill the watchdog before it has a chance to clean up.
			CreationFlags: 0x00000008,
		},
	}
	p, err := os.StartProcess(wdExe, []string{wdExe}, attr)
	if err != nil {
		_ = logFile.Close()
		util.LogWarn("tun: failed to spawn watchdog: %v", err)
		return
	}
	_ = logFile.Close()
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
// exit code via GetExitCodeProcess. Falls back to PROCESS_QUERY_LIMITED_INFORMATION
// if the full query handle is denied.
func processExists(pid int) bool {
	handle, err := syscall.OpenProcess(syscall.PROCESS_QUERY_INFORMATION, false, uint32(pid))
	if err != nil {
		handle, err = syscall.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
		if err != nil {
			return false
		}
	}
	defer syscall.CloseHandle(handle)
	var exitCode uint32
	if err := syscall.GetExitCodeProcess(handle, &exitCode); err != nil {
		return false
	}
	return exitCode == 259
}
