//go:build !windows

package tun

// Available reports whether TUN can be initialized on this platform.
// TUN is currently only supported on Windows.
func Available() bool {
	return false
}
