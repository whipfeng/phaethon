package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"phaethon/config"
	"phaethon/mesh"
	"phaethon/p2p"
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
// If meshMgr is non-nil, it is wired to the TUN engine immediately after start
// so that mesh can intercept packets even if later code in run() blocks.
func startTUNIfEnabled(ruleConf *config.RuleConfiguration, meshMgr *mesh.MeshManager) *TUNResource {
	// Clear the graceful-shutdown marker from any previous run.
	removeStoppedMarker()

	if !tun.Available() {
		return nil
	}

	// Clean up any residual TUN state from previous crashes before starting.
	tun.CleanupResidual()

	if ruleConf == nil || !ruleConf.TUN.IsEnabled() {
		util.LogInfo("TUN disabled by configuration")
		return nil
	}

	util.LogInfo("TUN enabled, initializing engine...")
	engine := tun.NewEngine(ruleConf)
	engine.SetDataDir(dataDir)
	if err := engine.Start(); err != nil {
		util.LogError("TUN engine start failed: %v", err)
		return nil
	}

	// Wire mesh to TUN engine immediately after start.
	// This must happen here (not in run()) because engine.Start() may block
	// on Windows in later steps, preventing run() from reaching the wiring code.
	if meshMgr != nil {
		// Set TUN reference FIRST to close the race where P2P receives mesh
		// frames before meshMgr.Start() is called below.
		meshMgr.SetTun(engine)

		meshVIP := meshMgr.GetVIP()
		engine.SetMeshInterceptor(meshMgr.HandleOutboundPacket, meshVIP)
		engine.SetMeshDNSResolver(meshMgr.ResolveMeshDomain)
		meshMgr.Start(engine, p2p.GlobalP2PManager)
		if meshVIP != nil {
			if err := engine.AddMeshVIP(meshVIP); err != nil {
				util.LogWarn("failed to add mesh VIP: %v", err)
			}
			// Add mesh VIP to OS interface so OS recognizes it as local
			go func() {
				time.Sleep(5 * time.Second)
				if err := engine.AddMeshVIPToOS(meshVIP); err != nil {
					util.LogWarn("failed to add mesh VIP to OS: %v", err)
				}
			}()
		}
		util.LogInfo("Mesh wired to TUN engine (vip=%s)", meshVIP)
	}

	return &TUNResource{engine: engine}
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
