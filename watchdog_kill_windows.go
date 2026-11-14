//go:build windows

package main

import (
	"os/exec"
	"strconv"
)

// killHandoffProcessTree kills the new watchdog and its worker. taskkill /T is
// required because Windows has no parent-death signal: killing only the
// watchdog would leave the worker holding the instance lock and ports.
func killHandoffProcessTree(pid int) {
	if pid <= 0 {
		return
	}
	_ = exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid)).Run()
}
