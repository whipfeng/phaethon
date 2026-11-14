//go:build !windows

package main

import (
	"os"
	"os/exec"
	"strconv"
	"time"
)

// killHandoffProcessTree kills the new watchdog and any worker it spawned.
// Linux workers get PR_SET_PDEATHSIG when their parent dies; pkill -P covers
// macOS and other Unix variants where that signal does not exist.
func killHandoffProcessTree(pid int) {
	if pid <= 0 {
		return
	}
	_ = exec.Command("pkill", "-TERM", "-P", strconv.Itoa(pid)).Run()
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
	time.Sleep(500 * time.Millisecond)
	_ = exec.Command("pkill", "-KILL", "-P", strconv.Itoa(pid)).Run()
}
