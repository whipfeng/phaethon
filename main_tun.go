package main

import (
	"os"
	"time"

	"phaethon/config"
	"phaethon/tun"
	"phaethon/util"
)

// TUNResource wraps a tun.Engine for lifecycle management.
type TUNResource struct {
	engine *tun.Engine
}

// Stop shuts down the TUN engine.
func (r *TUNResource) Stop() {
	if r.engine != nil {
		r.engine.Stop()
	}
}

// startTUNIfEnabled creates and starts the TUN engine when available and enabled.
// Detection order:
//  1. LAYER_WATCHDOG_PID set → run watchdog mode, never returns
//  2. TUN not available (no wintun.dll on Windows) → return nil
//  3. ruleConf.TUN explicitly disabled → return nil
//  4. Otherwise start TUN engine
func startTUNIfEnabled(ruleConf *config.RuleConfiguration) *TUNResource {
	// Watchdog mode: monitor parent process, cleanup on crash.
	// os.Exit ensures the child does not continue into normal startup.
	if wdPid := os.Getenv("LAYER_WATCHDOG_PID"); wdPid != "" {
		runWatchdog(wdPid)
		os.Exit(0)
	}

	if !tun.Available() {
		return nil
	}

	if ruleConf != nil && ruleConf.TUN != nil && !ruleConf.TUN.IsEnabled() {
		util.LogInfo("TUN disabled by configuration")
		return nil
	}

	util.LogInfo("TUN enabled, initializing engine...")
	engine := tun.NewEngine(ruleConf)
	if err := engine.Start(); err != nil {
		util.LogError("TUN engine start failed: %v", err)
		return nil
	}

	spawnWatchdog()
	return &TUNResource{engine: engine}
}

// runWatchdog blocks until the monitored parent process exits, then runs cleanup.
func runWatchdog(parentPID string) {
	pid := parsePID(parentPID)
	if pid <= 0 {
		return
	}
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if !processExists(pid) {
			util.LogInfo("tun-watchdog: parent process %d gone, cleaning up...", pid)
			tun.CleanupResidual()
			return
		}
	}
}

func parsePID(s string) int {
	var pid int
	for _, c := range s {
		if c >= '0' && c <= '9' {
			pid = pid*10 + int(c-'0')
		} else if pid > 0 {
			break
		}
	}
	return pid
}
