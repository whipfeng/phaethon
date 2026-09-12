package main

import (
	"encoding/json"
	"fmt"
	"net"
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

// startEngine creates and starts the engine.
// When TUN is enabled, it starts both the TUN device and gVisor netstack.
// When TUN is disabled but mesh is enabled, it starts only the gVisor netstack
// (no TUN device, no OS routes), allowing mesh gateway forwarding via InjectMeshPacket.
// If meshMgr is non-nil, it is wired to the engine immediately after start.
func startEngine(ruleConf *config.RuleConfiguration, meshMgr *mesh.MeshManager) *TUNResource {
	// Clear the graceful-shutdown marker from any previous run.
	removeStoppedMarker()

	tunEnabled := ruleConf != nil && ruleConf.TUN != nil && ruleConf.TUN.IsEnabled()
	meshEnabled := meshMgr != nil

	if !tunEnabled && !meshEnabled {
		return nil
	}

	// TUN device availability check (only needed when TUN is enabled)
	if tunEnabled {
		if !tun.Available() {
			util.LogWarn("TUN enabled but not available on this platform")
			return nil
		}
		// Clean up any residual TUN state from previous crashes before starting.
		tun.CleanupResidual()
	}

	if tunEnabled {
		util.LogInfo("TUN enabled, initializing engine...")
	} else {
		util.LogInfo("TUN disabled, starting gVisor netstack for mesh...")
	}

	engine := tun.NewEngine(ruleConf)
	engine.SetDataDir(dataDir)

	// Configure mesh addresses before Start() if mesh is enabled with a subnet
	if meshEnabled && meshMgr.GetSubnet() != "" {
		if _, subnet, err := net.ParseCIDR(meshMgr.GetSubnet()); err == nil {
			if err := engine.ConfigureMeshAddresses(subnet); err != nil {
				util.LogWarn("failed to configure mesh addresses: %v", err)
			}
		}
	}

	if err := engine.Start(); err != nil {
		util.LogError("Engine start failed: %v", err)
		return nil
	}

	// Wire mesh to engine immediately after start.
	// This must happen here (not in run()) because engine.Start() may block
	// on Windows in later steps, preventing run() from reaching the wiring code.
	if meshEnabled {
		// Set TUN reference FIRST to close the race where P2P receives mesh
		// frames before meshMgr.Start() is called below.
		meshMgr.SetTun(engine)

		meshVIP := meshMgr.GetVIP()
		allVIPs := meshMgr.GetAllVIPs()

		// Mesh interceptor is needed in both TUN and non-TUN modes:
		// - With TUN: intercepts outbound packets from readLoop
		// - Without TUN: used by meshWriteLoop to send responses back through mesh
		engine.SetMeshInterceptor(meshMgr.HandleOutboundPacket, allVIPs)

		engine.SetMeshDNSResolver(meshMgr.ResolveMeshDomain)
		engine.SetMeshDNSForwarder(meshMgr.MeshDNSForwarder)

		// Set up mesh DNS allocator (gateway allocates fakeIPs from local pool)
		if pool := engine.GetFakeIPPool(); pool != nil {
			meshMgr.DNSAllocator = func(domain string) (net.IP, error) {
				return pool.Lookup(domain), nil
			}
		}

		// Set up P2P DNS response handler
		p2p.GlobalP2PManager.SetMeshDNSResponseHandler(meshMgr.HandleDNSResponse)

		meshMgr.Start(engine, p2p.GlobalP2PManager)

		// Add all VIPs to OS interface so OS recognizes them as local (for source IP selection)
		// Only needed when TUN is enabled (VIPs are added to the TUN adapter)
		if tunEnabled {
			go func() {
				time.Sleep(5 * time.Second)
				for _, vip := range allVIPs {
					if err := engine.AddMeshVIPToOS(vip); err != nil {
						util.LogWarn("failed to add mesh VIP %s to OS: %v", vip, err)
					}
				}
			}()
		}
		util.LogInfo("Mesh wired to engine (vip=%s allVIPs=%v tunEnabled=%v)", meshVIP, allVIPs, tunEnabled)
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
