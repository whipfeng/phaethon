package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"

	"phaethon/config"
	"phaethon/dialer"
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
// The gVisor netstack (with DNS hijacker) is always started when there is a config,
// so that proxy servers (Mode B) can use netstack sockets for DNS and connections.
// When TUN is enabled, the TUN device and OS routes are also set up.
// If meshMgr is non-nil, it is wired to the engine immediately after start.
func startEngine(ruleConf *config.RuleConfiguration, meshMgr *mesh.MeshManager) *TUNResource {
	// Clear the graceful-shutdown marker from any previous run.
	removeStoppedMarker()

	if ruleConf == nil {
		return nil
	}

	tunEnabled := ruleConf.TUN != nil && ruleConf.TUN.IsEnabled()
	meshEnabled := meshMgr != nil

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
		// Wire Mode B (SOCKS5) netstack callbacks: DNS resolution and connection
		// dialing go through the netstack, which routes via loopback to the
		// hijacker/forwarder.
		dialer.GlobalNetstackDialFunc = engine.NetDial
		dialer.GlobalDNSResolverFunc = engine.ResolveDomain

		// Set TUN reference FIRST to close the race where P2P receives mesh
		// frames before meshMgr.Start() is called below.
		meshMgr.SetTun(engine)

		meshVIP := meshMgr.GetVIP()
		allVIPs := meshMgr.GetAllVIPs()

		// Mesh interceptor is needed in both TUN and non-TUN modes:
		// - With TUN: intercepts outbound packets from readLoop
		// - Without TUN: used by meshWriteLoop to send responses back through mesh
		engine.SetMeshInterceptor(meshMgr.HandleOutboundPacket, allVIPs)

		// Enable NAT and share the NATTable with the engine for TUN source/reverse NAT.
		meshMgr.EnableNAT()
		engine.SetNATTable(meshMgr.GetNATTable())

		engine.SetMeshDNSResolver(meshMgr.ResolveMeshDomain)
		engine.SetMeshDNSNetstackForwarder(meshMgr.ForwardDNSViaNetstack)

		// Set up mesh DNS allocator (gateway allocates fakeIPs from local pool)
		if pool := engine.GetFakeIPPool(); pool != nil {
			meshMgr.DNSAllocator = func(domain string) (net.IP, error) {
				return pool.Lookup(domain), nil
			}
		}

		// Set up DNS netstack forwarder callback (mesh -> engine's DNS hijacker)
		if hijacker := engine.GetDNSHijacker(); hijacker != nil {
			mesh.SetDNSNetstackForwarder(func(domain string, gatewayGIP net.IP) (net.IP, error) {
				return hijacker.ForwardViaNetstack(domain, gatewayGIP)
			})
		}

		meshMgr.Start(engine, p2p.GlobalP2PManager)

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
