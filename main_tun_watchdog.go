package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"phaethon/util"
)

// ensureWatchdogExecutable returns a path to a copy of the current executable
// with a "-watchdog" suffix in its basename. The separate filename prevents
// taskkill /IM, killall, and similar tools from matching both the parent
// process and the watchdog child in one shot.
//
// The copy is placed next to the main executable so it is reused across
// restarts. It is only recopied when the main binary changes (size/modtime).
func ensureWatchdogExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("get executable: %w", err)
	}

	dir := filepath.Dir(exe)
	ext := filepath.Ext(exe)
	base := filepath.Base(exe)
	if len(ext) > 0 && len(base) > len(ext) {
		base = base[:len(base)-len(ext)]
	}
	wdPath := filepath.Join(dir, base+"-watchdog"+ext)

	srcInfo, err := os.Stat(exe)
	if err != nil {
		return "", fmt.Errorf("stat executable: %w", err)
	}

	needCopy := true
	if dstInfo, err := os.Stat(wdPath); err == nil {
		if dstInfo.Size() == srcInfo.Size() && dstInfo.ModTime().Equal(srcInfo.ModTime()) {
			needCopy = false
		}
	}

	if needCopy {
		if err := copyFile(exe, wdPath); err != nil {
			// The existing file may be locked by a still-running watchdog.
			// If it exists, reuse it rather than failing startup.
			if _, statErr := os.Stat(wdPath); statErr == nil {
				util.LogWarn("tun: watchdog executable copy failed, reusing existing: %v", err)
				return wdPath, nil
			}
			return "", fmt.Errorf("copy watchdog executable: %w", err)
		}
		_ = os.Chtimes(wdPath, srcInfo.ModTime(), srcInfo.ModTime())
	}

	return wdPath, nil
}

func copyFile(src, dst string) error {
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
