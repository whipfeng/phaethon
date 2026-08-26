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

// runWatchdog monitors the parent process and the TUN DNS path. It cleans up
// when the parent dies, and kills the parent plus cleans up when the TUN DNS
// hijacker becomes unreachable from this separate process or when the TUN
// interface disappears.
func runWatchdog(parentPID string) {
	pid := parsePID(parentPID)
	if pid <= 0 {
		util.LogWarn("tun-watchdog: invalid parent pid %q, exiting", parentPID)
		return
	}

	util.LogInfo("tun-watchdog: started, monitoring parent %d", pid)

	const (
		procInterval    = 3 * time.Second
		dnsInterval     = 5 * time.Second
		ifaceInterval   = 5 * time.Second
		dnsGrace        = 30 * time.Second
		dnsFailLimit    = 5
		dnsProbeTimeout = 3 * time.Second
	)

	// During development/debugging the DNS-kill behavior can be disabled so the
	// process stays alive for manual inspection. Parent-death cleanup is always
	// kept so a crash does not strand the machine.
	noDNSKill := os.Getenv("LAYER_WATCHDOG_NO_DNS_KILL") == "1"

	procTicker := time.NewTicker(procInterval)
	defer procTicker.Stop()
	dnsTicker := time.NewTicker(dnsInterval)
	defer dnsTicker.Stop()
	ifaceTicker := time.NewTicker(ifaceInterval)
	defer ifaceTicker.Stop()

	// Give the parent a grace period to finish TUN setup before probing DNS.
	enableDNSProbe := time.After(dnsGrace)
	dnsFailCount := 0

	for {
		select {
		case <-enableDNSProbe:
			enableDNSProbe = nil

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

		case <-dnsTicker.C:
			if enableDNSProbe != nil {
				continue
			}
			if tun.ProbeTUNDNS(dnsProbeTimeout) {
				dnsFailCount = 0
				continue
			}
			dnsFailCount++
			util.LogWarn("tun-watchdog: DNS probe failed (%d/%d)", dnsFailCount, dnsFailLimit)
			if dnsFailCount >= dnsFailLimit {
				if noDNSKill {
					util.LogWarn("tun-watchdog: DNS probe failed %d times, but LAYER_WATCHDOG_NO_DNS_KILL=1 keeps parent alive", dnsFailCount)
					dnsFailCount = 0
					continue
				}
				util.LogError("tun-watchdog: TUN DNS unreachable, killing parent %d and cleaning up", pid)
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
