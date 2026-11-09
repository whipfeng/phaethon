//go:build windows

package main

import "os"

// gracefulExitSignal on Windows must be os.Kill: os.Process.Signal rejects
// SIGTERM, so asking for it would silently fail and force the watchdog to wait
// the full grace period before terminating the child anyway.
func gracefulExitSignal() os.Signal {
	return os.Kill
}
