//go:build !linux && !windows

package p2p

import (
	"phaethon/util"
)

// PerformSelfUpdate is a stub for unsupported platforms.
func PerformSelfUpdate(newBinaryPath, version string) {
	util.LogModuleInfo("p2p", "self-update: not supported on this platform, binary cached only")
}
