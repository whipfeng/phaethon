//go:build !windows && !linux && !darwin

package tun

// setSystemDNS is a no-op on unsupported platforms.
func setSystemDNS(ifaceName, tunIP string) error {
	return nil
}

// restoreSystemDNS is a no-op on unsupported platforms.
func restoreSystemDNS(ifaceName string) {
}

// dnsRegistrationIface is a no-op helper on unsupported platforms.
func dnsRegistrationIface(tunName, physicalName string) string {
	return physicalName
}
