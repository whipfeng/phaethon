package main

import (
	"net"
	"os"
	"strings"
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

	probeURLs := []string(nil)
	if ruleConf != nil && ruleConf.TUN != nil {
		probeURLs = ruleConf.TUN.ProbeURLList()
	}
	spawnWatchdog(probeURLs)
	return &TUNResource{engine: engine}
}

// probeURLsFromEnv parses the LAYER_WATCHDOG_PROBE_URLS environment variable.
// Semicolon-separated URLs are treated as explicit probe targets. An empty or
// unset variable means the watchdog should use tun.DefaultProbeURLs.
func probeURLsFromEnv() []string {
	raw := os.Getenv("LAYER_WATCHDOG_PROBE_URLS")
	if raw == "" {
		return nil
	}
	return strings.Split(raw, ";")
}

// getCurrentTUNInterfaceIndex returns the current interface index of the
// phaethontun adapter. This is used by the watchdog to dynamically bind to
// the correct interface, avoiding stale indices when the adapter is recreated.
func getCurrentTUNInterfaceIndex() int {
	iface, err := net.InterfaceByName("phaethontun")
	if err != nil {
		return 0
	}
	return iface.Index
}

// runWatchdog monitors the parent process and the TUN outbound path. It cleans up
// when the parent dies, and kills the parent plus cleans up when the TUN HTTP
// probe becomes unreachable from this separate process or when the TUN interface
// disappears.
func runWatchdog(parentPID string) {
	pid := parsePID(parentPID)
	if pid <= 0 {
		util.LogWarn("tun-watchdog: invalid parent pid %q, exiting", parentPID)
		return
	}

	util.LogInfo("tun-watchdog: started, monitoring parent %d", pid)

	const (
		procInterval   = 3 * time.Second
		probeInterval  = 30 * time.Second
		ifaceInterval  = 5 * time.Second
		probeFailLimit = 10
		dnsTimeout     = 5 * time.Second
		httpTimeout    = 30 * time.Second
	)

	probeURLs := probeURLsFromEnv()

	// The watchdog verifies real outbound connectivity by sending HTTP probes
	// through the TUN adapter. DNS resolution uses a pure Go resolver to avoid
	// OS thread blocking on Windows.
	probe := func() bool {
		// Dynamically get current TUN interface index to avoid binding to stale interfaces
		currentIfIndex := getCurrentTUNInterfaceIndex()
		if currentIfIndex <= 0 {
			util.LogWarn("tun-watchdog: cannot determine current TUN interface index")
			return false
		}
		return tun.ProbeTUNHTTPWithBind(dnsTimeout, httpTimeout, currentIfIndex, probeURLs)
	}
	util.LogInfo("tun-watchdog: using HTTP probe with dynamic interface binding")

	procTicker := time.NewTicker(procInterval)
	defer procTicker.Stop()
	probeTicker := time.NewTicker(probeInterval)
	defer probeTicker.Stop()
	ifaceTicker := time.NewTicker(ifaceInterval)
	defer ifaceTicker.Stop()

	util.LogInfo("tun-watchdog: entering main loop")

	probeFailCount := 0

	for {
		select {
		case <-procTicker.C:
			if !processExists(pid) {
				util.LogInfo("tun-watchdog: parent process %d gone, cleaning up...", pid)
				tun.CleanupResidual()
				return
			}

		case <-ifaceTicker.C:
			if !tun.InterfaceExists() {
				util.LogError("tun-watchdog: TUN interface missing, killing parent %d and cleaning up", pid)
				killParentAndCleanup(pid)
				return
			}

		case <-probeTicker.C:
			if probe() {
				probeFailCount = 0
				continue
			}
			probeFailCount++
			util.LogWarn("tun-watchdog: probe failed (%d/%d)", probeFailCount, probeFailLimit)
			if probeFailCount >= probeFailLimit {
				util.LogError("tun-watchdog: probe unreachable, killing parent %d and cleaning up", pid)
				killParentAndCleanup(pid)
				return
			}
		}
	}
}

func killParentAndCleanup(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
	// Wait briefly for the parent to exit so its own cleanup can run first.
	time.Sleep(2 * time.Second)
	tun.CleanupResidual()
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
