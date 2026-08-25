//go:build !windows && !linux && !darwin

package tun

// setSystemDNS is a no-op on unsupported platforms.
func setSystemDNS(devName, tunIP string) error {
	return nil
}

// restoreSystemDNS is a no-op on unsupported platforms.
func restoreSystemDNS(devName string) {
}
