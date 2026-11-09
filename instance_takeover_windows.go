//go:build windows

package main

import "fmt"

// takeoverInstanceLock is not implemented on Windows: verifying that the lock
// owner is this deployment requires process-image inspection that adds little
// value here. Stop the running instance (service manager or Task Manager)
// before starting a new one.
func takeoverInstanceLock(string) error {
	return fmt.Errorf("--takeover is not supported on Windows; stop the running instance first")
}
