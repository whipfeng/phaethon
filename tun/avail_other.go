//go:build !windows && !linux && !darwin

package tun

// Available reports whether TUN can be initialized on this platform.
// Linux and Darwin have their own runtime checks; everything else is unsupported.
func Available() bool {
	return false
}
