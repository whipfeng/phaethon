//go:build windows

package p2p

import (
	"io"
	"os"

	"phaethon/util"
)

// performSelfUpdate replaces the running binary on Windows.
// Windows allows renaming a running exe but not deleting/overwriting it.
// After replacing files, exits with code 42 to signal the watchdog to perform
// the watchdog handoff (spawn new watchdog, old watchdog exits).
func performSelfUpdate(newBinaryPath, version, platform, arch, buildTag string) {
	execPath, err := os.Executable()
	if err != nil {
		util.LogError("[P2P] self-update: get executable path: %v", err)
		return
	}
	backupPath := execPath + ".bak"

	// Windows allows renaming a running exe
	if err := os.Rename(execPath, backupPath); err != nil {
		util.LogError("[P2P] self-update: backup %s → %s: %v", execPath, backupPath, err)
		return
	}
	util.LogInfo("[P2P] self-update: backed up %s → %s", execPath, backupPath)

	// Copy new binary to original path (now free since we renamed it)
	if err := copyFileLocal(newBinaryPath, execPath); err != nil {
		util.LogError("[P2P] self-update: copy %s → %s: %v", newBinaryPath, execPath, err)
		os.Rename(backupPath, execPath)
		return
	}

	util.LogInfo("[P2P] self-update: replaced binary, exiting with code 42 for watchdog handoff")

	// Exit with code 42 — watchdog sees this and spawns a new watchdog
	os.Exit(42)
}

// cleanupBackup removes the .bak file after the new watchdog starts.
func cleanupBackup() {
	execPath, err := os.Executable()
	if err != nil {
		return
	}
	bakPath := execPath + ".bak"
	if err := os.Remove(bakPath); err != nil && !os.IsNotExist(err) {
		util.LogDebug("[P2P] cleanup backup: %v", err)
	} else if err == nil {
		util.LogInfo("[P2P] cleanup: removed %s", bakPath)
	}
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
