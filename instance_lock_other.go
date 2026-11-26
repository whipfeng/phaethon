//go:build !linux && !darwin && !windows

package main

// acquireInstanceLock is a no-op on platforms without a supported file-lock
// primitive. The watchdog still supervises its child; only cross-process
// single-instance protection is unavailable.
func acquireInstanceLock(string) (func(), error) {
	return func() {}, nil
}

// probeInstanceLock always reports the lock as free: there is no cross-process
// lock on this platform, so the rollback wait is a no-op.
func probeInstanceLock(string) bool { return true }
