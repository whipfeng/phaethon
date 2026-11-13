//go:build !windows

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// installBinary replaces exe with newPath atomically on POSIX. The previous
// executable is preserved as a hardlink (or copy) at backupPath, so the stable
// path is never absent: there is no window for a crash/power loss to leave the
// launcher with nothing to start.
func installBinary(exe, newPath, backupPath string) error {
	_ = os.Remove(backupPath)
	if err := os.Link(exe, backupPath); err != nil {
		// Filesystems without hardlink support: fall back to a copy.
		if err := copyBinaryFile(exe, backupPath); err != nil {
			return fmt.Errorf("preserve backup: %w", err)
		}
	}
	if err := os.Rename(newPath, exe); err != nil {
		return fmt.Errorf("atomic replace: %w", err)
	}
	return nil
}

// syncParentDir flushes the directory entry after a rename. Best effort:
// some filesystems/platforms reject directory fsync.
func syncParentDir(path string) {
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

func copyBinaryFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return os.Chmod(dst, 0755)
}
