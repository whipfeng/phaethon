//go:build windows

package main

import (
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

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

// reapChild on Windows retrieves the exit code of a terminated process.
func reapChild(pid int) (exitCode int, ok bool) {
	handle, err := syscall.OpenProcess(syscall.PROCESS_QUERY_INFORMATION, false, uint32(pid))
	if err != nil {
		handle, err = syscall.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
		if err != nil {
			return 0, false
		}
	}
	defer syscall.CloseHandle(handle)
	var code uint32
	if err := syscall.GetExitCodeProcess(handle, &code); err != nil {
		return 0, false
	}
	if code == 259 { // STILL_ACTIVE
		return 0, false
	}
	return int(code), true
}

// waitForProcessExit waits for a process to exit using WaitForSingleObject.
func waitForProcessExit(pid int, timeout time.Duration) bool {
	handle, err := syscall.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(handle)
	millis := uint32(timeout.Milliseconds())
	if millis == 0 {
		millis = 1
	}
	k32 := syscall.NewLazyDLL("kernel32.dll")
	waitFunc := k32.NewProc("WaitForSingleObject")
	ret, _, _ := waitFunc.Call(uintptr(handle), uintptr(millis))
	return ret == 0 // WAIT_OBJECT_0
}

// killResidualWorkers is a no-op on Windows. The watchdog does not run on
// Windows, and orphaned worker processes are not expected there.
func killResidualWorkers() {}
