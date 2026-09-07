//go:build darwin

package main

// setProcessName is a no-op on macOS. The kernel does not support renaming
// the process visible in ps; pthread_setname_np only renames the current thread.
func setProcessName(name string) {}
