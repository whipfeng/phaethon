//go:build linux

package p2p

import (
	"io"
	"os"
	"syscall"

	"phaethon/util"
)

// PerformSelfUpdate replaces the running binary on Linux using execve().
// The process image is replaced in-place — same PID, watchdog is unaware.
func PerformSelfUpdate(newBinaryPath, version string) {
	execPath, err := os.Executable()
	if err != nil {
		util.LogError("[UPDATE] get executable path: %v", err)
		return
	}
	backupPath := execPath + ".bak"

	util.LogInfo("[UPDATE] self-update: %s → %s from %s", runningVersion(), version, newBinaryPath)

	if err := os.Chmod(newBinaryPath, 0755); err != nil {
		util.LogError("[UPDATE] chmod %s: %v", newBinaryPath, err)
		return
	}

	// Linux allows renaming a running binary
	if err := os.Rename(execPath, backupPath); err != nil {
		util.LogError("[UPDATE] backup %s → %s: %v", execPath, backupPath, err)
		return
	}
	util.LogInfo("[UPDATE] backed up %s → %s", execPath, backupPath)

	// Copy new binary to original path
	if err := copyFileLocal(newBinaryPath, execPath); err != nil {
		util.LogError("[UPDATE] copy %s → %s: %v", newBinaryPath, execPath, err)
		os.Rename(backupPath, execPath)
		return
	}
	if err := os.Chmod(execPath, 0755); err != nil {
		util.LogError("[UPDATE] chmod %s: %v", execPath, err)
		os.Remove(execPath)
		os.Rename(backupPath, execPath)
		return
	}

	util.LogInfo("[UPDATE] replaced binary, executing in-place (execve)")

	// execve replaces the process image — same PID, watchdog unaware
	// After execve, the new process will clean up .bak on startup
	if err := syscall.Exec(execPath, os.Args, os.Environ()); err != nil {
		util.LogError("[UPDATE] execve failed: %v, rolling back", err)
		os.Remove(execPath)
		os.Rename(backupPath, execPath)
		os.Exit(1)
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
