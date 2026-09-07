package main

import (
	"encoding/json"
	"fmt"
	"net"
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
// The watchdog is no longer spawned here — it runs as the parent process.
func startTUNIfEnabled(ruleConf *config.RuleConfiguration) *TUNResource {
	// Clear the graceful-shutdown marker from any previous run.
	removeStoppedMarker()

	if !tun.Available() {
		return nil
	}

	if ruleConf == nil || !ruleConf.TUN.IsEnabled() {
		util.LogInfo("TUN disabled by configuration")
		return nil
	}

	util.LogInfo("TUN enabled, initializing engine...")
	engine := tun.NewEngine(ruleConf)
	if err := engine.Start(); err != nil {
		util.LogError("TUN engine start failed: %v", err)
		return nil
	}

	return &TUNResource{engine: engine}
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

// waitForExit waits up to timeout for a process to exit.
func waitForExit(pid int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processExists(pid) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

const stoppedMarkerPath = "/var/run/phaethon.stopped"

func writeStoppedMarker() {
	_ = os.WriteFile(stoppedMarkerPath, []byte("1"), 0644)
}

func removeStoppedMarker() {
	_ = os.Remove(stoppedMarkerPath)
}

func wasStoppedGracefully() bool {
	_, err := os.Stat(stoppedMarkerPath)
	return err == nil
}

// emitProtocolMsg writes a JSON line to stdout for the watchdog to read.
// Only used when running as a worker child (PHAETHON_WORKER=1).
func emitProtocolMsg(msg interface{}) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	fmt.Fprintln(os.Stdout, string(data))
}

// startWorkerHeartbeat sends periodic heartbeat messages to the watchdog.
// Called once after the worker signals ready.
func startWorkerHeartbeat() {
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			emitProtocolMsg(map[string]bool{"heartbeat": true})
		}
	}()
}
