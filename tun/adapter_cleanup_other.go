//go:build !windows

package tun

// deleteTUNAdapter is a no-op: Linux/Darwin TUN devices disappear with their
// file descriptor, so there is no persisted adapter to remove.
func deleteTUNAdapter(string) {}
