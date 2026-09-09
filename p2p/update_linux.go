//go:build linux

package p2p

import (
	"io"
	"os"
	"syscall"

	"phaethon/util"
)

// performSelfUpdate replaces the running binary on Linux using execve().
// The process image is replaced in-place — same PID, watchdog is unaware.
func performSelfUpdate(newBinaryPath, version, platform, arch, buildTag string) {
	execPath, err := os.Executable()
	if err != nil {
		util.LogError("[P2P] self-update: get executable path: %v", err)
		return
	}
	backupPath := execPath + ".bak"

	if err := os.Chmod(newBinaryPath, 0755); err != nil {
		util.LogError("[P2P] self-update: chmod %s: %v", newBinaryPath, err)
		return
	}

	// Linux allows renaming a running binary
	if err := os.Rename(execPath, backupPath); err != nil {
		util.LogError("[P2P] self-update: backup %s → %s: %v", execPath, backupPath, err)
		return
	}
	util.LogInfo("[P2P] self-update: backed up %s → %s", execPath, backupPath)

	// Copy new binary to original path
	if err := copyFileLocal(newBinaryPath, execPath); err != nil {
		util.LogError("[P2P] self-update: copy %s → %s: %v", newBinaryPath, execPath, err)
		os.Rename(backupPath, execPath)
		return
	}
	if err := os.Chmod(execPath, 0755); err != nil {
		util.LogError("[P2P] self-update: chmod %s: %v", execPath, err)
		os.Remove(execPath)
		os.Rename(backupPath, execPath)
		return
	}

	util.LogInfo("[P2P] self-update: replaced binary, executing in-place (execve)")

	// execve replaces the process image — same PID, watchdog unaware
	// After execve, the new process will clean up .bak on startup
	if err := syscall.Exec(execPath, os.Args, os.Environ()); err != nil {
		util.LogError("[P2P] self-update: execve failed: %v, rolling back", err)
		os.Remove(execPath)
		os.Rename(backupPath, execPath)
		os.Exit(1)
	}
}

// cleanupBackup removes the .bak file after successful execve restart.
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
