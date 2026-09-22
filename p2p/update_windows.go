//go:build windows

package p2p

import (
	"io"
	"os"

	"phaethon/util"
)

// PerformSelfUpdate replaces the running binary on Windows.
// Windows allows renaming a running exe but not deleting/overwriting it.
// After replacing files, exits with code 42 to signal the watchdog to perform
// the watchdog handoff (spawn new watchdog, old watchdog exits).
func PerformSelfUpdate(newBinaryPath, version string) {
	execPath, err := os.Executable()
	if err != nil {
		util.LogError("[UPDATE] get executable path: %v", err)
		return
	}
	backupPath := execPath + ".bak"

	util.LogInfo("[UPDATE] self-update: %s → %s from %s", runningVersion(), version, newBinaryPath)

	// Windows allows renaming a running exe
	if err := os.Rename(execPath, backupPath); err != nil {
		util.LogError("[UPDATE] backup %s → %s: %v", execPath, backupPath, err)
		return
	}
	util.LogInfo("[UPDATE] backed up %s → %s", execPath, backupPath)

	// Copy new binary to original path (now free since we renamed it)
	if err := copyFileLocal(newBinaryPath, execPath); err != nil {
		util.LogError("[UPDATE] copy %s → %s: %v", newBinaryPath, execPath, err)
		os.Rename(backupPath, execPath)
		return
	}

	util.LogInfo("[UPDATE] replaced binary, exiting with code 42 for watchdog handoff")

	// Exit with code 42 — watchdog sees this and spawns a new watchdog
	os.Exit(42)
}

func copyFileLocal(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}
