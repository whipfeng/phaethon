//go:build windows

package tun

import (
	"os"
	"path/filepath"

	"phaethon/util"
)

// Available reports whether TUN can be initialized on this platform.
// Detection order:
//  1. TUN_ENABLED=true → force enable
//  2. TUN_ENABLED=false → force disable
//  3. wintun.dll exists beside the executable → auto-enable (Windows only)
//  4. wintun.dll exists in working directory → auto-enable (for watchdog/worker split)
//  5. Otherwise false.
func Available() bool {
	// Explicit env var takes precedence
	if os.Getenv("TUN_ENABLED") == "true" {
		return true
	}
	if os.Getenv("TUN_ENABLED") == "false" {
		return false
	}
	// Auto-detect: check for wintun.dll on Windows
	// First check beside the executable (standard case)
	exe, err := os.Executable()
	if err == nil {
		dir := filepath.Dir(exe)
		if _, err := os.Stat(filepath.Join(dir, "wintun.dll")); err == nil {
			util.LogInfo("wintun.dll found beside executable, TUN is available")
			return true
		}
	}
	// Also check working directory (for watchdog/worker split where worker
	// binary is in data/worker/ but wintun.dll is in main directory)
	if cwd, err := os.Getwd(); err == nil {
		if _, err := os.Stat(filepath.Join(cwd, "wintun.dll")); err == nil {
			util.LogInfo("wintun.dll found in working directory, TUN is available")
			return true
		}
	}
	return false
}
