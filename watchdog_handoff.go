package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"phaethon/util"
)

// Watchdog handoff: after a self-upgrade the old watchdog starts a new
// watchdog from the freshly installed binary, waits for its worker to become
// ready, and only then exits. The new process takes over supervision, so
// watchdog code is updated too and the old image file is unlocked when the
// old process exits.
//
// The instance lock is held by the worker, not the watchdog (see
// instance_lock_unix.go). Stopping the old worker releases it; the new
// watchdog's worker takes it. No lock transfer is required.

const (
	handoffEnvToken  = "PHAETHON_WATCHDOG_HANDOFF"
	handoffEnvTarget = "PHAETHON_WATCHDOG_TARGET"

	handoffRequestPath  = "data/state/watchdog-handoff-request.json"
	handoffResultPath   = "data/state/watchdog-handoff-result.json"
	watchdogVersionPath = "data/state/watchdog-version"

	handoffRequestStarting = "starting"
	handoffRequestAbort    = "abort"

	handoffResultAccepted = "accepted"
	handoffResultReady    = "ready"
	handoffResultFailed   = "failed"

	handoffReadyTimeout = 45 * time.Second
	handoffAbortWait    = 10 * time.Second
)

var errHandoffStopped = errors.New("watchdog handoff aborted: shutdown requested")

type handoffRequest struct {
	Token         string    `json:"token"`
	OldPID        int       `json:"oldPid"`
	OldVersion    string    `json:"oldVersion"`
	TargetVersion string    `json:"targetVersion"`
	Status        string    `json:"status"`
	Reason        string    `json:"reason,omitempty"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type handoffResult struct {
	Token      string    `json:"token"`
	NewPID     int       `json:"newPid"`
	NewVersion string    `json:"newVersion"`
	Status     string    `json:"status"`
	Error      string    `json:"error,omitempty"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

func newHandoffToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func shortToken(token string) string {
	if len(token) > 8 {
		return token[:8]
	}
	return token
}

func writeHandoffJSON(path string, v interface{}) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readHandoffRequest() *handoffRequest {
	var req handoffRequest
	data, err := os.ReadFile(handoffRequestPath)
	if err != nil || json.Unmarshal(data, &req) != nil || req.Token == "" {
		return nil
	}
	return &req
}

func readHandoffResult() *handoffResult {
	var res handoffResult
	data, err := os.ReadFile(handoffResultPath)
	if err != nil || json.Unmarshal(data, &res) != nil || res.Token == "" {
		return nil
	}
	return &res
}

func clearHandoffFiles() {
	_ = os.Remove(handoffRequestPath)
	_ = os.Remove(handoffResultPath)
}

func publishHandoffResult(token, status, version, errMsg string) {
	req := readHandoffRequest()
	if req == nil || req.Token != token || req.Status == handoffRequestAbort {
		return
	}
	res := handoffResult{
		Token:      token,
		NewPID:     os.Getpid(),
		NewVersion: version,
		Status:     status,
		Error:      errMsg,
		UpdatedAt:  time.Now(),
	}
	if err := writeHandoffJSON(handoffResultPath, res); err != nil {
		util.LogModuleWarn("main", "watchdog: write handoff result: %v", err)
	}
}

func handoffAbortRequested(token string) bool {
	req := readHandoffRequest()
	return req != nil && req.Token == token && req.Status == handoffRequestAbort
}

func writeWatchdogVersion(version string) {
	_ = os.MkdirAll(filepath.Dir(watchdogVersionPath), 0755)
	_ = os.WriteFile(watchdogVersionPath, []byte(version+"\n"), 0644)
}

// handoffEnv builds the new watchdog's environment. PHAETHON_WORKER must not
// leak into it, otherwise the new process would start as a worker instead of
// supervising one.
func handoffEnv(token, target string) []string {
	src := os.Environ()
	out := make([]string, 0, len(src)+2)
	for _, kv := range src {
		if strings.HasPrefix(kv, "PHAETHON_WORKER=") ||
			strings.HasPrefix(kv, handoffEnvToken+"=") ||
			strings.HasPrefix(kv, handoffEnvTarget+"=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, handoffEnvToken+"="+token, handoffEnvTarget+"="+target)
}

// handoffToNewWatchdog starts exe as a new watchdog, waits for its worker to
// become ready, and returns nil. The caller must not start another worker and
// should exit on success.
func handoffToNewWatchdog(exe, targetVersion string, stopping *atomic.Bool) error {
	token := newHandoffToken()
	req := handoffRequest{
		Token:         token,
		OldPID:        os.Getpid(),
		OldVersion:    Version,
		TargetVersion: targetVersion,
		Status:        handoffRequestStarting,
		UpdatedAt:     time.Now(),
	}
	if err := writeHandoffJSON(handoffRequestPath, req); err != nil {
		return fmt.Errorf("write handoff request: %w", err)
	}

	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		clearHandoffFiles()
		return fmt.Errorf("open %s: %w", os.DevNull, err)
	}
	defer devnull.Close()

	proc, err := os.StartProcess(exe, append([]string{exe}, os.Args[1:]...), &os.ProcAttr{
		Env:   handoffEnv(token, targetVersion),
		Files: []*os.File{devnull, devnull, devnull},
	})
	if err != nil {
		clearHandoffFiles()
		return fmt.Errorf("start handoff watchdog: %w", err)
	}
	// Reap the child if it exits during the handoff. On success the old
	// watchdog exits first and the new process is adopted by init/the OS.
	go func() { _, _ = proc.Wait() }()
	upgradeLog("handoff: started new watchdog pid=%d target=%s", proc.Pid, targetVersion)

	deadline := time.Now().Add(handoffReadyTimeout)
	for time.Now().Before(deadline) {
		if res := readHandoffResult(); res != nil && res.Token == token {
			switch res.Status {
			case handoffResultReady:
				if res.NewVersion == targetVersion {
					clearHandoffFiles()
					upgradeLog("handoff: new watchdog ready pid=%d version=%s", res.NewPID, res.NewVersion)
					return nil
				}
				reason := fmt.Sprintf("new watchdog reported version %q, want %q", res.NewVersion, targetVersion)
				abortHandoff(token, proc.Pid, reason)
				return errors.New(reason)
			case handoffResultFailed:
				abortHandoff(token, proc.Pid, res.Error)
				return fmt.Errorf("new watchdog failed: %s", res.Error)
			}
		}
		if stopping != nil && stopping.Load() {
			abortHandoff(token, proc.Pid, "shutdown requested")
			return errHandoffStopped
		}
		if !processExists(proc.Pid) {
			abortHandoff(token, proc.Pid, "new watchdog exited before ready")
			return errors.New("new watchdog exited before ready")
		}
		time.Sleep(200 * time.Millisecond)
	}

	reason := fmt.Sprintf("new watchdog not ready within %v", handoffReadyTimeout)
	abortHandoff(token, proc.Pid, reason)
	return errors.New(reason)
}

// abortHandoff asks the new watchdog to stop its worker and exit. If it does
// not comply, the process tree is killed so the instance lock is released
// before the old watchdog rolls back.
func abortHandoff(token string, pid int, reason string) {
	upgradeLog("handoff: abort pid=%d reason=%s", pid, reason)
	if req := readHandoffRequest(); req != nil && req.Token == token {
		req.Status = handoffRequestAbort
		req.Reason = reason
		req.UpdatedAt = time.Now()
		_ = writeHandoffJSON(handoffRequestPath, req)
	}

	if pid > 0 {
		deadline := time.Now().Add(handoffAbortWait)
		for time.Now().Before(deadline) && processExists(pid) {
			time.Sleep(200 * time.Millisecond)
		}
		if processExists(pid) {
			upgradeLog("handoff: new watchdog %d did not exit, killing process tree", pid)
			killHandoffProcessTree(pid)
			time.Sleep(500 * time.Millisecond)
		}
	}
	clearHandoffFiles()
}
