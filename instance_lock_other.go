//go:build !linux && !darwin && !windows

package main

// acquireInstanceLock is a no-op on platforms without a supported file-lock
// primitive. The watchdog still supervises its child; only cross-process
// single-instance protection is unavailable.
func acquireInstanceLock(string) (func(), error) {
	return func() {}, nil
}
