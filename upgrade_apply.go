package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"phaethon/pkg/signing"
	"phaethon/pkg/upgrade"
	"phaethon/util"
)

// Self-replacement constants. The executable path never changes: the watchdog
// renames the current binary aside, moves the staged binary into the same path,
// and restarts it. Windows Firewall / AV allowlists keyed on the full program
// path therefore stay valid.
const (
	// workerReadyWait bounds the post-swap readiness check.
	workerReadyWait = 20 * time.Second
	// workerStopGrace is how long a worker gets to exit after the graceful
	// signal before the watchdog force-kills it.
	workerStopGrace = 10 * time.Second
)

// stagedPath returns the staging/backup path next to the stable executable.
func stagedPath(exe, suffix string) string { return exe + suffix }

// findPkgForVersion locates a signed package in data/packages that matches this
// build's platform/arch and the requested version.
func findPkgForVersion(version string) (string, *signing.PkgContents, error) {
	const packagesDir = "data/packages"
	entries, err := os.ReadDir(packagesDir)
	if err != nil {
		return "", nil, fmt.Errorf("read %s: %w", packagesDir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".pkg") {
			continue
		}
		pkgPath := filepath.Join(packagesDir, entry.Name())
		contents, err := signing.VerifyPkg(pkgPath, signing.PublicKey)
		if err != nil {
			util.LogModuleDebug("main", "watchdog: skip pkg %s: %v", entry.Name(), err)
			continue
		}
		if contents.Meta.Version != version {
			continue
		}
		if contents.Meta.Platform != Platform || contents.Meta.Arch != Arch {
			continue
		}
		return pkgPath, contents, nil
	}
	return "", nil, fmt.Errorf("no signed package for %s (%s/%s)", version, Platform, Arch)
}

// stageBinary writes the package binary next to the executable as phaethon.new.
func stageBinary(exe string, contents *signing.PkgContents) (string, error) {
	newPath := stagedPath(exe, ".new")
	_ = os.Remove(newPath)
	if err := os.WriteFile(newPath, contents.Binary, 0755); err != nil {
		return "", fmt.Errorf("write staged binary: %w", err)
	}
	return newPath, nil
}

// swapBinary installs newPath at the stable executable path, keeping the
// previous binary as .old and the one before that as .old2.
func swapBinary(exe, newPath string) error {
	oldPath := stagedPath(exe, ".old")
	old2Path := stagedPath(exe, ".old2")

	if fileExists(oldPath) {
		_ = os.Remove(old2Path)
		if err := os.Rename(oldPath, old2Path); err != nil {
			return fmt.Errorf("rotate backup: %w", err)
		}
	}
	if err := os.Rename(exe, oldPath); err != nil {
		return fmt.Errorf("move current binary aside: %w", err)
	}
	if err := os.Rename(newPath, exe); err != nil {
		// Best effort: put the original back so the service can still start.
		_ = os.Rename(oldPath, exe)
		return fmt.Errorf("install staged binary: %w", err)
	}
	return nil
}

// restoreBackup puts .old back at the stable path. A bad binary that is
// currently in place is kept as .failed for diagnosis.
func restoreBackup(exe string) error {
	oldPath := stagedPath(exe, ".old")
	if !fileExists(oldPath) {
		return fmt.Errorf("no .old backup to restore")
	}
	failedPath := stagedPath(exe, ".failed")
	_ = os.Remove(failedPath)
	if fileExists(exe) {
		if err := os.Rename(exe, failedPath); err != nil {
			return fmt.Errorf("move failed binary aside: %w", err)
		}
	}
	if err := os.Rename(oldPath, exe); err != nil {
		return fmt.Errorf("restore backup: %w", err)
	}
	return nil
}

// repairInterruptedUpgrade runs before the first spawn. It closes the window
// where a crash/power loss between the renames can leave the stable path
// missing.
func repairInterruptedUpgrade(exe string) {
	marker := upgrade.ReadMarker()
	_, exeErr := os.Stat(exe)
	oldPath := stagedPath(exe, ".old")
	newPath := stagedPath(exe, ".new")
	newExists := fileExists(newPath)

	if exeErr == nil {
		// The swap completed (or never started). A leftover .new is stale.
		if newExists {
			_ = os.Remove(newPath)
		}
		if marker != nil {
			util.LogModuleWarn("main", "watchdog: upgrade marker present but executable exists; assuming swap completed (target=%s)", marker.Version)
			_ = upgrade.ClearMarker()
			st := upgrade.ReadStatus()
			if st.Status != upgrade.StatusSucceeded {
				_ = upgrade.WriteStatus(upgrade.Status{
					Status:    upgrade.StatusApplying,
					Current:   upgrade.ReadWorkerVersion(),
					Requested: marker.Version,
				})
			}
		}
		return
	}

	if fileExists(oldPath) {
		if err := os.Rename(oldPath, exe); err != nil {
			util.LogModuleError("main", "watchdog: executable missing and backup restore failed: %v", err)
			return
		}
		util.LogModuleWarn("main", "watchdog: executable missing at startup; restored .old backup")
		if newExists {
			_ = os.Remove(newPath)
		}
		_ = upgrade.ClearRequest() // do not retry a swap that already broke once
		_ = upgrade.ClearMarker()
		requested := ""
		if marker != nil {
			requested = marker.Version
		}
		_ = upgrade.WriteStatus(upgrade.Status{
			Status:    upgrade.StatusFailedRolledBack,
			Current:   upgrade.ReadWorkerVersion(),
			Requested: requested,
			LastError: "executable missing at startup; restored backup",
		})
		return
	}

	if newExists {
		// No backup exists (first install). The staged binary is all we have.
		if err := os.Rename(newPath, exe); err != nil {
			util.LogModuleError("main", "watchdog: executable missing and staged binary restore failed: %v", err)
			return
		}
		util.LogModuleWarn("main", "watchdog: executable missing at startup; installed staged .new binary")
		requested := ""
		if marker != nil {
			requested = marker.Version
		}
		_ = upgrade.WriteStatus(upgrade.Status{
			Status:    upgrade.StatusApplying,
			Requested: requested,
		})
	}
}

// stopChild gracefully stops the worker and reaps it.
//
// The wait is on the real process, not the stdout pipe: the pipe can close
// slightly before the process is reaped, and the old upgrade path called a
// non-blocking Wait4 at that moment, leaving a zombie child behind on Unix.
func stopChild(cp *childProcess, pid int) {
	if cp == nil || cp.proc == nil {
		return
	}
	cp.kill(gracefulExitSignal())

	waitDone := make(chan struct{})
	go func() {
		_, _ = cp.proc.Wait()
		close(waitDone)
	}()

	select {
	case <-waitDone:
		return
	case <-time.After(workerStopGrace):
		util.LogModuleWarn("main", "watchdog: worker %d did not exit within %v, killing", pid, workerStopGrace)
		cp.kill(os.Kill)
		select {
		case <-waitDone:
		case <-time.After(5 * time.Second):
			util.LogModuleError("main", "watchdog: worker %d still not reaped after SIGKILL", pid)
		}
	}
}

// spawnAndWait starts exe as the worker and waits for its ready message.
func spawnAndWait(exe string, timeout time.Duration) (*childProcess, error) {
	cp, err := spawnChildProcess(exe)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		if cp.ready.Load() {
			return cp, nil
		}
		select {
		case <-cp.done:
			return cp, fmt.Errorf("worker exited before signaling ready")
		default:
		}
		if time.Now().After(deadline) {
			return cp, fmt.Errorf("worker not ready within %v", timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// markUpgradeSucceeded records a completed upgrade and clears the request.
func markUpgradeSucceeded(requested, current string) {
	_ = upgrade.WriteStatus(upgrade.Status{
		Status:    upgrade.StatusSucceeded,
		Current:   current,
		Requested: requested,
	})
	_ = upgrade.WriteWorkerVersion(current)
	_ = upgrade.ClearRequest()
	_ = upgrade.ClearMarker()
}

// failUpgradeBeforeSwap reports a failure that never touched the binaries.
// The running worker is left untouched.
func failUpgradeBeforeSwap(current string, req *upgrade.Request, err error) {
	util.LogModuleError("main", "watchdog: upgrade to %s not applied: %v", req.Version, err)
	_ = upgrade.WriteStatus(upgrade.Status{
		Status:    upgrade.StatusFailedRolledBack,
		Current:   current,
		Requested: req.Version,
		LastError: err.Error(),
	})
	_ = upgrade.ClearRequest()
	_ = upgrade.ClearMarker()
}

// rollbackUpgrade restores the backup and starts it again. The request is
// cleared so a broken package is not retried in a loop.
func rollbackUpgrade(exe string, fallback *childProcess, fromVersion string, req *upgrade.Request, reason error) (*childProcess, int) {
	util.LogModuleError("main", "watchdog: upgrade to %s failed, rolling back to %s: %v", req.Version, fromVersion, reason)
	if err := restoreBackup(exe); err != nil {
		util.LogModuleError("main", "watchdog: rollback restore failed: %v", err)
	}
	_ = upgrade.ClearRequest()
	_ = upgrade.ClearMarker()

	st := upgrade.Status{
		Status:    upgrade.StatusFailedRolledBack,
		Current:   fromVersion,
		Requested: req.Version,
		LastError: reason.Error(),
	}

	cp, err := spawnAndWait(exe, workerReadyWait)
	if err != nil {
		util.LogModuleError("main", "watchdog: rolled-back worker did not become ready: %v", err)
		_ = upgrade.WriteStatus(st)
		if cp != nil {
			return cp, cp.proc.Pid
		}
		if fallback != nil && fallback.proc != nil {
			return fallback, fallback.proc.Pid
		}
		return nil, 0
	}
	if v := cp.workerVersion(); v != "" {
		st.Current = v
		_ = upgrade.WriteWorkerVersion(v)
	}
	_ = upgrade.WriteStatus(st)
	return cp, cp.proc.Pid
}

// tryApplyUpgrade applies a pending request. handled=false means there is no
// request. When handled=true the caller owns the returned child (nil means the
// existing child was left untouched).
func tryApplyUpgrade(exe string, cp *childProcess, pid int) (*childProcess, int, bool) {
	req := upgrade.ReadRequest()
	if req == nil {
		return nil, 0, false
	}

	running := ""
	if cp != nil {
		running = cp.workerVersion()
	}
	if running == "" {
		running = upgrade.ReadWorkerVersion()
	}

	// Idempotent: a watchdog restart can re-observe a request that already
	// took effect.
	if running != "" && running == req.Version {
		util.LogModuleInfo("main", "watchdog: already running requested version %s", req.Version)
		markUpgradeSucceeded(req.Version, running)
		return nil, 0, true
	}

	// Wait until the current worker is healthy before touching it.
	if cp == nil || !cp.ready.Load() {
		return nil, 0, true
	}

	util.LogModuleInfo("main", "watchdog: applying upgrade %s (running %s)", req.Version, running)
	_ = upgrade.WriteStatus(upgrade.Status{
		Status:    upgrade.StatusApplying,
		Current:   running,
		Requested: req.Version,
	})

	// Verify and stage before stopping the worker: a missing/corrupt package
	// must not interrupt service.
	_, contents, err := findPkgForVersion(req.Version)
	if err != nil {
		failUpgradeBeforeSwap(running, req, err)
		return nil, 0, true
	}
	newPath, err := stageBinary(exe, contents)
	if err != nil {
		failUpgradeBeforeSwap(running, req, err)
		return nil, 0, true
	}

	_ = upgrade.WriteMarker(upgrade.Marker{
		Old:     stagedPath(exe, ".old"),
		New:     newPath,
		Version: req.Version,
	})

	stopChild(cp, pid)

	if err := swapBinary(exe, newPath); err != nil {
		newCp, newPid := rollbackUpgrade(exe, cp, running, req, err)
		return newCp, newPid, true
	}

	newCp, err := spawnAndWait(exe, workerReadyWait)
	if err != nil {
		stopChild(newCp, cpPid(newCp))
		rbCp, rbPid := rollbackUpgrade(exe, cp, running, req, err)
		return rbCp, rbPid, true
	}
	if got := newCp.workerVersion(); got != req.Version {
		stopChild(newCp, cpPid(newCp))
		rbCp, rbPid := rollbackUpgrade(exe, cp, running, req, fmt.Errorf("worker reported version %q, want %q", got, req.Version))
		return rbCp, rbPid, true
	}

	util.LogModuleInfo("main", "watchdog: upgrade to %s succeeded (pid=%d)", req.Version, newCp.proc.Pid)
	markUpgradeSucceeded(req.Version, req.Version)
	_ = upgrade.WriteWorkerVersion(req.Version)
	return newCp, newCp.proc.Pid, true
}

func cpPid(cp *childProcess) int {
	if cp == nil || cp.proc == nil {
		return 0
	}
	return cp.proc.Pid
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
