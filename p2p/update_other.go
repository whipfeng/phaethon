//go:build !linux && !windows

package p2p

import (
	"phaethon/util"
)

// performSelfUpdate is a stub for unsupported platforms.
func performSelfUpdate(newBinaryPath, version, platform, arch, buildTag string) {
	util.LogInfo("[P2P] self-update: not supported on this platform, binary cached only")
}

// cleanupBackup is a no-op on unsupported platforms.
func cleanupBackup() {}
