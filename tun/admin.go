//go:build !windows

package tun

// EnsureAdminPrivileges is a no-op on non-Windows platforms.
// Linux/macOS TUN typically requires root/sudo, which is handled externally.
func EnsureAdminPrivileges() error {
	return nil
}
