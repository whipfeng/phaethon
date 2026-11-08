//go:build !windows

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"phaethon/util"
)

// consoleCloseNotify returns a channel that is never closed on non-Windows
// platforms. Console close events are handled via normal signals there.
func consoleCloseNotify() <-chan struct{} {
	return make(chan struct{})
}

// processExists checks whether a process with the given PID is still running.
func processExists(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil
}

// reapChild reaps a zombie child process and returns its exit code.
// The watchdog becomes the parent of the server process, so it must
// wait on it to avoid zombies.
func reapChild(pid int) (exitCode int, ok bool) {
	var ws syscall.WaitStatus
	_, err := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
	if err != nil {
		return 0, false
	}
	if ws.Exited() {
		return ws.ExitStatus(), true
	}
	return 0, false
}

// reexecAsWatchdog copies the current binary to a "-watchdog" suffixed name
// and re-execs into it via syscall.Exec. The PID stays the same so init
// systems (OpenRC, systemd) continue tracking the process normally.
// If already running as the watchdog copy, returns immediately.
func reexecAsWatchdog() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	dir := filepath.Dir(exe)
	ext := filepath.Ext(exe)
	base := filepath.Base(exe)
	if len(ext) > 0 && len(base) > len(ext) {
		base = base[:len(base)-len(ext)]
	}
	if base == "phaethon-watchdog" {
		return
	}
	wdPath := filepath.Join(dir, "phaethon-watchdog"+ext)
	if err := copyFile(exe, wdPath); err != nil {
		if _, statErr := os.Stat(wdPath); statErr == nil {
			// Already exists (possibly locked), reuse it.
		} else {
			return
		}
	}
	_ = syscall.Exec(wdPath, []string{wdPath}, os.Environ())
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

// procSnapshot 是一次 /proc/<pid> 采样结果；用于精确识别"本部署的 phaethon 残留"。
type procSnapshot struct {
	pid int
	exe string // 已去掉 " (deleted)" 后缀
	cwd string
	uid int // -1 表示读取失败
}

// readProcSnapshot 读取进程的可执行文件、工作目录与 real uid。
// 读不到 exe 时返回错误，调用方应保守放行（宁可少杀，不可误杀）。
func readProcSnapshot(pid int) (procSnapshot, error) {
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return procSnapshot{}, err
	}
	exe = strings.TrimSuffix(exe, " (deleted)")
	cwd, _ := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))

	uid := -1
	if status, rerr := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid)); rerr == nil {
		for _, line := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(line, "Uid:") {
				if f := strings.Fields(line); len(f) >= 2 {
					if v, aerr := strconv.Atoi(f[1]); aerr == nil {
						uid = v
					}
				}
				break
			}
		}
	}
	return procSnapshot{pid: pid, exe: exe, cwd: cwd, uid: uid}, nil
}

// isResidualWorker 判断进程是否属于"本部署的 phaethon"（watchdog 或 worker）。
//
// 必须同时满足：
//  1. 可执行文件名以 phaethon 开头
//  2. uid 与本进程一致（读不到时跳过该条件）
//  3. 可执行文件位于本部署目录，或位于 <cwd>/data/worker/，或 cwd+二进制名完全相同
//
// 这样可避免误杀 `tail -f phaethon.log`、部署脚本、以及同机其他部署实例。
func isResidualWorker(p procSnapshot, selfExe, selfDir, selfCwd string, selfUID int) bool {
	base := filepath.Base(p.exe)
	if !strings.HasPrefix(base, "phaethon") {
		return false
	}
	if p.uid >= 0 && selfUID >= 0 && p.uid != selfUID {
		return false
	}
	// pkg worker 二进制：<cwd>/data/worker/phaethon-<version>
	if filepath.Dir(p.exe) == filepath.Join(selfCwd, "data", "worker") {
		return true
	}
	// 与 watchdog 同目录（phaethon / phaethon-watchdog）
	if filepath.Dir(p.exe) == selfDir {
		return true
	}
	// cwd 相同且二进制同名（应对软链、重新部署后路径差异）
	if p.cwd != "" && p.cwd == selfCwd && base == filepath.Base(selfExe) {
		return true
	}
	return false
}

// signalAndWait 先 SIGTERM，宽限 3s 后对仍存活的发 SIGKILL，最后 reap。
func signalAndWait(pids []int) {
	if len(pids) == 0 {
		return
	}
	for _, pid := range pids {
		if p, err := os.FindProcess(pid); err == nil {
			if err := p.Signal(syscall.SIGTERM); err != nil {
				util.LogModuleDebug("main", "watchdog: SIGTERM pid %d failed: %v", pid, err)
			}
		}
	}
	time.Sleep(3 * time.Second)
	for _, pid := range pids {
		if processExists(pid) {
			util.LogModuleWarn("main", "watchdog: pid %d still alive after SIGTERM, sending SIGKILL", pid)
			if p, err := os.FindProcess(pid); err == nil {
				_ = p.Signal(syscall.SIGKILL)
			}
		}
	}
	time.Sleep(1 * time.Second)
	for _, pid := range pids {
		reapChild(pid)
	}
}

// killResidualWorkers finds and kills orphaned phaethon processes belonging to
// THIS deployment only — matched by executable path + cwd + uid, not by a
// substring of cmdline (which used to kill `tail -f phaethon.log` and
// deployment shells). Must be called BEFORE spawning a new child so the new
// child is never mistaken for a residual.
//
// Kill order matters: watchdogs first. On Linux the worker carries a
// parent-death signal, so it exits on its own; killing workers first would let
// the old watchdog respawn a fresh worker that is not in the residual list.
func killResidualWorkers() {
	if _, err := os.Stat("/proc"); err != nil {
		return
	}
	selfExe, err := os.Executable()
	if err != nil {
		util.LogModuleWarn("main", "watchdog: cannot resolve own executable, skip residual scan: %v", err)
		return
	}
	selfDir := filepath.Dir(selfExe)
	selfCwd, _ := os.Getwd()
	selfUID := os.Getuid()
	workerDir := filepath.Join(selfCwd, "data", "worker")

	myPid := os.Getpid()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}

	var residualWatchdogs, residualWorkers []int
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, convErr := strconv.Atoi(entry.Name())
		if convErr != nil || pid == myPid {
			continue
		}
		snap, snapErr := readProcSnapshot(pid)
		if snapErr != nil {
			// 权限不足 / 已退出 / 容器 hidepid：保守放行
			util.LogModuleDebug("main", "watchdog: skip pid %d: cannot read exe (%v)", pid, snapErr)
			continue
		}
		if strings.Contains(snap.exe, "supervise-daemon") {
			continue
		}
		if !isResidualWorker(snap, selfExe, selfDir, selfCwd, selfUID) {
			continue
		}
		util.LogModuleInfo("main", "watchdog: residual candidate pid=%d exe=%s cwd=%s uid=%d",
			snap.pid, snap.exe, snap.cwd, snap.uid)
		if filepath.Dir(snap.exe) == workerDir {
			residualWorkers = append(residualWorkers, pid)
		} else {
			residualWatchdogs = append(residualWatchdogs, pid)
		}
	}

	if len(residualWatchdogs)+len(residualWorkers) == 0 {
		return
	}
	util.LogModuleInfo("main", "watchdog: killing %d residual watchdog(s) first: %v", len(residualWatchdogs), residualWatchdogs)
	signalAndWait(residualWatchdogs)
	if len(residualWorkers) > 0 {
		util.LogModuleInfo("main", "watchdog: cleaning up %d residual worker(s): %v", len(residualWorkers), residualWorkers)
		signalAndWait(residualWorkers)
	}
	util.LogModuleInfo("main", "watchdog: residual cleanup done")
}
