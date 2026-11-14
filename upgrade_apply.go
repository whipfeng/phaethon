package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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

// upgradeLog writes an unfiltered audit line to data/logs/upgrade.log. The
// watchdog's normal module logger defaults to Error, which would hide every
// successful apply/rollback step; this file is the upgrade audit trail.
func upgradeLog(format string, args ...interface{}) {
	path := filepath.Join("data", "logs", "upgrade.log")
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		util.LogModuleWarn("main", "watchdog: cannot write upgrade log: %v", err)
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, args...))
}

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
// The data is fsynced before returning: the rename-based install is only
// crash-safe if the staged contents are durable.
func stageBinary(exe string, contents *signing.PkgContents) (string, error) {
	newPath := stagedPath(exe, ".new")
	_ = os.Remove(newPath)
	f, err := os.OpenFile(newPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return "", fmt.Errorf("create staged binary: %w", err)
	}
	if _, err := f.Write(contents.Binary); err != nil {
		f.Close()
		_ = os.Remove(newPath)
		return "", fmt.Errorf("write staged binary: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(newPath)
		return "", fmt.Errorf("sync staged binary: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(newPath)
		return "", fmt.Errorf("close staged binary: %w", err)
	}
	if err := os.Chmod(newPath, 0755); err != nil {
		_ = os.Remove(newPath)
		return "", fmt.Errorf("chmod staged binary: %w", err)
	}
	syncParentDir(newPath)
	return newPath, nil
}

// prepareBackup rotates the backup chain and returns the path that
// installBinary may use as the backup of the current executable.
//
// The rotation target .old2 can be the image of the still-running watchdog:
// the first upgrade after a watchdog start usually has no .old and skips
// rotation entirely; the next upgrade must free .old2, but Windows allows
// renaming a mapped image and rejects deleting/overwriting it ("Access is
// denied"). The old code ignored the failed remove and aborted on the rename,
// leaving every Windows node stuck one version behind. Keep .old in place and
// use a fixed secondary backup path instead; ReplaceFileW overwrites an
// existing backup file.
func prepareBackup(exe string) string {
	oldPath := stagedPath(exe, ".old")
	if !fileExists(oldPath) {
		return oldPath
	}
	old2Path := stagedPath(exe, ".old2")
	_ = os.Remove(old2Path)
	if err := os.Rename(oldPath, old2Path); err == nil {
		return oldPath
	} else {
		util.LogModuleWarn("main", "watchdog: cannot rotate .old (%v); keeping it and using .old.last", err)
		upgradeLog("rotate backup failed: %v; using .old.last", err)
		return stagedPath(exe, ".old.last")
	}
}

// swapBinary installs newPath at the stable executable path. installBinary is
// platform-specific: POSIX replaces the directory entry atomically; Windows
// uses ReplaceFileW when available and saves the previous executable at
// backupPath.
func swapBinary(exe, newPath, backupPath string) error {
	if err := installBinary(exe, newPath, backupPath); err != nil {
		return err
	}
	syncParentDir(exe)
	return nil
}

// restoreBackup puts a backup back at the stable path. The backup actually
// used by the swap is tried first: on Windows it may be .old.last when .old is
// the watchdog's own mapped image. A bad binary that is currently in place is
// kept as .failed for diagnosis.
func restoreBackup(exe, backupPath string) error {
	candidates := make([]string, 0, 3)
	if backupPath != "" {
		candidates = append(candidates, backupPath)
	}
	candidates = append(candidates, stagedPath(exe, ".old"), stagedPath(exe, ".old2"))

	src := ""
	for _, c := range candidates {
		if fileExists(c) {
			src = c
			break
		}
	}
	if src == "" {
		return fmt.Errorf("no backup to restore")
	}
	failedPath := stagedPath(exe, ".failed")
	_ = os.Remove(failedPath)
	if fileExists(exe) {
		if err := os.Rename(exe, failedPath); err != nil {
			return fmt.Errorf("move failed binary aside: %w", err)
		}
	}
	if err := os.Rename(src, exe); err != nil {
		return fmt.Errorf("restore backup %s: %w", src, err)
	}
	return nil
}

// minRestoredBinarySize rejects zero/partial files left by an interrupted
// write or an AV quarantine before they are installed as the stable binary.
const minRestoredBinarySize = 1 << 20

// tryRestoreCandidate renames a backup/staged candidate into the stable path if
// it looks like a real binary. It returns the candidate label and true when the
// stable path is restored.
func tryRestoreCandidate(exe, label, candidate string) (string, bool) {
	fi, err := os.Stat(candidate)
	if err != nil || fi.IsDir() || fi.Size() < minRestoredBinarySize {
		return label, false
	}
	if err := os.Rename(candidate, exe); err != nil {
		util.LogModuleWarn("main", "watchdog: restore %s failed: %v", candidate, err)
		return label, false
	}
	return label, true
}

// repairInterruptedUpgrade runs before the first spawn. It closes the window
// where a crash/power loss between the renames can leave the stable path
// missing.
func repairInterruptedUpgrade(exe string) {
	marker := upgrade.ReadMarker()
	_, exeErr := os.Stat(exe)
	newPath := stagedPath(exe, ".new")
	newExists := fileExists(newPath)

	if exeErr == nil {
		// The swap completed (or never started). A leftover .new is stale.
		if newExists {
			_ = os.Remove(newPath)
		}
		if marker != nil {
			util.LogModuleWarn("main", "watchdog: upgrade marker present but executable exists; assuming swap completed (target=%s)", marker.Version)
			upgradeLog("repair: marker present, executable exists; assume swap completed target=%s", marker.Version)
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

	// Stable path is missing. Try the backup recorded in the marker first (it
	// may be .old.last on Windows), then the historical chain, then .new. A
	// restored .new keeps the request so the normal ready/version check can
	// complete it; a restored backup clears the request (do not loop a swap
	// that already broke once).
	type candidate struct {
		label string
		path  string
	}
	candidates := []candidate{}
	if marker != nil && marker.Old != "" {
		candidates = append(candidates, candidate{"marker old", marker.Old})
	}
	candidates = append(candidates,
		candidate{".old", stagedPath(exe, ".old")},
		candidate{".old2", stagedPath(exe, ".old2")},
		candidate{".new", stagedPath(exe, ".new")},
	)
	seen := map[string]bool{}
	for _, c := range candidates {
		if seen[c.path] {
			continue
		}
		seen[c.path] = true
		label, ok := tryRestoreCandidate(exe, c.label, c.path)
		if !ok {
			continue
		}
		util.LogModuleWarn("main", "watchdog: executable missing at startup; restored %s", label)
		upgradeLog("repair: executable missing; restored %s", label)
		if label == ".new" {
			requested := ""
			if marker != nil {
				requested = marker.Version
			}
			_ = upgrade.WriteStatus(upgrade.Status{
				Status:    upgrade.StatusApplying,
				Requested: requested,
			})
			return
		}
		_ = upgrade.ClearRequest()
		_ = upgrade.ClearMarker()
		_ = os.Remove(newPath)
		requested := ""
		if marker != nil {
			requested = marker.Version
		}
		_ = upgrade.WriteStatus(upgrade.Status{
			Status:    upgrade.StatusFailedRolledBack,
			Current:   upgrade.ReadWorkerVersion(),
			Requested: requested,
			LastError: "executable missing at startup; restored " + label,
		})
		return
	}

	util.LogModuleError("main", "watchdog: executable missing at startup and no usable .old/.old2/.new backup")
	upgradeLog("repair: executable missing and no usable backup")
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
	upgradeLog("succeeded: target=%s current=%s", requested, current)
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
	upgradeLog("apply failed before swap: target=%s current=%s err=%v", req.Version, current, err)
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
func rollbackUpgrade(exe string, fallback *childProcess, fromVersion string, req *upgrade.Request, backupPath string, reason error) (*childProcess, int) {
	util.LogModuleError("main", "watchdog: upgrade to %s failed, rolling back to %s: %v", req.Version, fromVersion, reason)
	upgradeLog("rollback begin: target=%s from=%s reason=%v", req.Version, fromVersion, reason)
	if err := restoreBackup(exe, backupPath); err != nil {
		util.LogModuleError("main", "watchdog: rollback restore failed: %v", err)
		upgradeLog("rollback restore failed: %v", err)
	} else {
		upgradeLog("rollback restored backup=%s", backupPath)
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
		upgradeLog("rollback worker not ready: %v", err)
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
	upgradeLog("rollback succeeded: running=%s pid=%d", st.Current, cp.proc.Pid)
	_ = upgrade.WriteStatus(st)
	return cp, cp.proc.Pid
}

// tryApplyUpgrade applies a pending request. handled=false means there is no
// request. When handled=true the caller owns the returned child (nil means the
// existing child was left untouched).
func tryApplyUpgrade(exe string, cp *childProcess, pid int, stopping *atomic.Bool) (*childProcess, int, bool) {
	req := upgrade.ReadRequest()
	if req == nil {
		return nil, 0, false
	}
	upgradeLog("request observed: target=%s", req.Version)

	// Idempotence is decided only by the live child. The worker-version file is
	// a display/status hint; using it here could declare success while the
	// current binary is crash-looping.
	live := ""
	if cp != nil && cp.ready.Load() {
		live = cp.workerVersion()
	}
	current := live
	if current == "" {
		current = upgrade.ReadWorkerVersion()
	}
	if live != "" && live == req.Version {
		util.LogModuleInfo("main", "watchdog: already running requested version %s", req.Version)
		upgradeLog("request already satisfied: target=%s", req.Version)
		markUpgradeSucceeded(req.Version, live)
		return nil, 0, true
	}

	// Wait until the current worker is healthy before touching it.
	if cp == nil || !cp.ready.Load() {
		return nil, 0, true
	}

	util.LogModuleInfo("main", "watchdog: applying upgrade %s (running %s)", req.Version, current)
	upgradeLog("applying: target=%s current=%s", req.Version, current)
	_ = upgrade.WriteStatus(upgrade.Status{
		Status:    upgrade.StatusApplying,
		Current:   current,
		Requested: req.Version,
	})

	// Verify and stage before stopping the worker: a missing/corrupt package
	// must not interrupt service.
	_, contents, err := findPkgForVersion(req.Version)
	if err != nil {
		failUpgradeBeforeSwap(current, req, err)
		return nil, 0, true
	}
	newPath, err := stageBinary(exe, contents)
	if err != nil {
		failUpgradeBeforeSwap(current, req, err)
		return nil, 0, true
	}
	upgradeLog("staged: target=%s path=%s bytes=%d", req.Version, newPath, len(contents.Binary))

	// Choose the backup path before writing the marker: repair-on-startup
	// must know where the previous executable went.
	backupPath := prepareBackup(exe)
	_ = upgrade.WriteMarker(upgrade.Marker{
		Old:     backupPath,
		New:     newPath,
		Version: req.Version,
	})
	upgradeLog("marker written: target=%s backup=%s", req.Version, backupPath)

	if stopping != nil && stopping.Load() {
		upgradeLog("aborted before stop: shutdown requested")
		_ = os.Remove(newPath)
		_ = upgrade.ClearMarker()
		_ = upgrade.WriteStatus(upgrade.Status{
			Status:    upgrade.StatusIdle,
			Current:   current,
			Requested: req.Version,
		})
		return nil, 0, true
	}

	upgradeLog("stopping worker: pid=%d", pid)
	stopChild(cp, pid)

	if err := swapBinary(exe, newPath, backupPath); err != nil {
		upgradeLog("swap failed: %v", err)
		newCp, newPid := rollbackUpgrade(exe, cp, current, req, backupPath, err)
		return newCp, newPid, true
	}
	upgradeLog("swap complete: target=%s", req.Version)

	newCp, err := spawnAndWait(exe, workerReadyWait)
	if err != nil {
		upgradeLog("new worker not ready: %v", err)
		stopChild(newCp, cpPid(newCp))
		rbCp, rbPid := rollbackUpgrade(exe, cp, current, req, backupPath, err)
		return rbCp, rbPid, true
	}
	if got := newCp.workerVersion(); got != req.Version {
		upgradeLog("new worker version mismatch: got=%s want=%s", got, req.Version)
		stopChild(newCp, cpPid(newCp))
		rbCp, rbPid := rollbackUpgrade(exe, cp, current, req, backupPath, fmt.Errorf("worker reported version %q, want %q", got, req.Version))
		return rbCp, rbPid, true
	}

	upgradeLog("ready: target=%s pid=%d", req.Version, newCp.proc.Pid)
	util.LogModuleInfo("main", "watchdog: upgrade to %s succeeded (pid=%d)", req.Version, newCp.proc.Pid)
	markUpgradeSucceeded(req.Version, req.Version)
	_ = upgrade.WriteWorkerVersion(req.Version)

	// Honor a shutdown request that arrived during the swap: stop the new
	// child and let the monitor loop see stopping + closed stdout and exit.
	if stopping != nil && stopping.Load() {
		upgradeLog("shutdown requested during apply; stopping new worker")
		stopChild(newCp, newCp.proc.Pid)
		return nil, 0, true
	}
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
