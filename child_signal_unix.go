//go:build linux || darwin

package main

import (
	"os"
	"syscall"
)

// gracefulExitSignal is the signal the watchdog sends before restarting or
// stopping its child. Unix children install a SIGTERM handler that releases
// the TUN device, routes and DNS changes.
func gracefulExitSignal() os.Signal {
	return syscall.SIGTERM
}
