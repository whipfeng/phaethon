package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"phaethon/config"
	"phaethon/db"
	"phaethon/dialer"
	"phaethon/mesh"
	"phaethon/handler"
	"phaethon/p2p"
	"phaethon/tun"
	"phaethon/util"
)

// TUNResource wraps a tun.Engine for lifecycle management.
type TUNResource struct {
	engine   *tun.Engine
	netstack *mesh.Netstack
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
// If adminHandler is non-nil, it is set for direct connection handling (bypasses OS network stack).
func startEngine(meshMgr *mesh.MeshManager, adminHandler tun.AdminHandler) *TUNResource {
	// Clear the graceful-shutdown marker from any previous run.
	removeStoppedMarker()

	// Load TUN config directly from database
	tunConfig, err := db.GetTUNConfig()
	if err != nil {
		util.LogModuleWarn("engine", "failed to load TUN config: %v, defaulting to disabled", err)
		tunConfig = nil
	}

	tunEnabled := tunConfig != nil && tunConfig.IsEnabled()

	// TUN device availability check (only needed when TUN is enabled)
	if tunEnabled {
		if !tun.Available() {
			util.LogModuleWarn("engine", "TUN enabled but not available on this platform, falling back to TUN-disabled mode for mesh")
			tunEnabled = false
		} else {
			// Clean up any residual TUN state from previous crashes before starting.
			tun.CleanupResidual()
		}
	}

	if tunEnabled {
		util.LogModuleInfo("engine", "TUN enabled, initializing engine...")
	} else {
		util.LogModuleInfo("engine", "TUN disabled, starting gVisor netstack for mesh...")
	}

	engine := tun.NewEngine()
	engine.SetDataDir(dataDir)

	// Create and set the netstack before starting the engine
	ns := mesh.NewNetstack()
	engine.SetNetstack(ns)

	// Configure mesh addresses before Start()
	if meshMgr.GetSubnet() != "" {
		if _, subnet, err := net.ParseCIDR(meshMgr.GetSubnet()); err == nil {
			if err := engine.ConfigureMeshAddresses(subnet); err != nil {
				util.LogModuleWarn("engine", "failed to configure mesh addresses: %v", err)
			}
		}
	}
	if network := meshMgr.GetNetwork(); network != nil {
		engine.SetMeshNetwork(network)
	}

	// Inject phaethon's module logging into gVisor fork diagnostic logs.
	// Route gVisor diagnostic logs to Debug level to reduce noise.
	// Per-packet logs (gvisor, ipip, route, etc.) should not flood at Info level.
	stack.SetLogFunc(func(module string, format string, args ...interface{}) {
		util.LogModuleDebug(module, format, args...)
	})

	if err := engine.Start(); err != nil {
		util.LogModuleError("engine", "Engine start failed: %v", err)
		return nil
	}

	// Check if TUN is actually running (it may have failed silently and fallen back to mesh-only)
	if tunEnabled && !engine.IsTUNRunning() {
		util.LogModuleWarn("engine", "TUN was enabled in config but failed to start - running in mesh-only mode")
		util.LogModuleWarn("engine", "Local applications cannot use the proxy, but mesh routing still works")
	}

	// Wire mesh to engine immediately after start.
	// This must happen here (not in run()) because engine.Start() may block
	// on Windows in later steps, preventing run() from reaching the wiring code.
	{
		// Create Mode B connection tracking table early (needed by dialer functions)
		modeBTable := mesh.NewModeBTable()

		// Bind mesh's DNS hijacker to the netstack.
		// This must happen after engine.Start() (netstack is ready) and before
		// any DNS queries are made.
		util.LogModuleInfo("engine", "[DNS-INIT] About to bind DNS hijacker, dnsHijacker=%v, fakeIPPool=%v", meshMgr.GetDNSHijacker() != nil, meshMgr.GetFakeIPPool() != nil)
		ns.SetDNSHijacker(meshMgr.GetDNSHijacker(), meshMgr.GetFakeIPPool())
		if err := ns.BindNetstack(); err != nil {
			util.LogModuleError("engine", "Failed to bind DNS hijacker to netstack: %v", err)
			return nil
		}
		if err := ns.StartDNSHijacker(); err != nil {
			util.LogModuleError("engine", "Failed to start DNS hijacker: %v", err)
			return nil
		}
		util.LogModuleInfo("engine", "[DNS-INIT] DNS hijacker bound and started")

		// Register the domain resolver callback for mesh DNS resolution
		ns.SetDNSDomainResolver(meshMgr.ResolveDomainSubnet)

		// Wire Mode B (SOCKS5) netstack callbacks: DNS resolution and connection
		// dialing go through the netstack, which routes via loopback to the
		// hijacker/forwarder.
		dialer.GlobalNetstackDialWithModeBFunc = func(network, addr string, clientAddr string, inbound string, mapping *config.Mapping) (net.Conn, error) {
			return ns.NetDialWithModeB(network, addr, clientAddr, inbound, mapping, modeBTable)
		}
		dialer.GlobalNetstackDialV2Func = ns.NetDialV2
		dialer.GlobalDNSResolverFunc = ns.ResolveDomain
		dialer.GlobalRemoteDNSResolverFunc = ns.ResolveDomainViaDNS

		// Set netstack reference FIRST to close the race where P2P receives mesh
		// frames before meshMgr.Start() is called below.
		meshMgr.SetNetstack(ns)

		meshVIP := meshMgr.GetVIP()
		allVIPs := meshMgr.GetAllVIPs()

		// Phase 2: Mesh interceptor retired - all traffic goes through netstack routing
		// Tunnel NICs handle IPIP encapsulation, route table directs traffic appropriately
		ns.SetMeshConfig(allVIPs)

		// Share Mode B table globally and with netstack
		ns.SetModeBTable(modeBTable)
		dialer.GlobalModeBTable = modeBTable

		ns.SetLocalMeshNodeID(meshMgr.GetNodeID())

		// Set admin handler for direct connection handling (bypasses OS network stack)
		if adminHandler != nil {
			engine.SetAdminHandler(adminHandler)
			util.LogModuleInfo("engine", "Admin handler set for direct connection handling")
		}

		// Create and set connection handler for TCP/UDP connection processing
		// Convert allVIPs slice to map for ConnectionHandler
		localMeshVIPsMap := make(map[string]bool)
		for _, vip := range allVIPs {
			localMeshVIPsMap[vip.String()] = true
		}
		
		connHandler := handler.NewConnectionHandler(handler.ConnectionHandlerConfig{
			FakeIP:           meshMgr.GetFakeIPPool(),
			LocalMeshNodeID:  meshMgr.GetNodeID(),
			LocalMeshVIPs:    localMeshVIPsMap,
			AdminHandler:     adminHandler,
			ModeBTable:       modeBTable,
			TUNConfig:        tunConfig,
			IsMeshIP:         ns.IsMeshIP,
			ResolveForDirect: engine.ResolveForDirect,
		})
		
		// Set up forwarder callbacks directly on netstack
		ns.SetCallbacks(&mesh.ForwarderCallbacks{
			HandleTCPConn: func(conn net.Conn, srcAddr string, srcPort int, dstAddr string, dstPort int, inbound string, modeBMapping *config.Mapping) {
				if connHandler != nil {
					connHandler.HandleTCPConn(conn, srcAddr, srcPort, dstAddr, dstPort, inbound, modeBMapping)
				}
			},
			HandleUDPConn: func(conn net.Conn, srcAddr, dstAddr string, dstPort int, inbound string, modeBMapping *config.Mapping) {
				if connHandler != nil {
					connHandler.HandleUDPConn(conn, srcAddr, dstAddr, dstPort, inbound, modeBMapping)
				}
			},
			LookupModeB: func(proto int, dstIP net.IP, dstPort, srcPort uint16) (string, string, *config.Mapping) {
				if modeBTable != nil {
					return modeBTable.LookupByDst(byte(proto), dstIP, dstPort, srcPort)
				}
				return "", "", nil
			},
			IsMeshIP: ns.IsMeshIP,
			StatsNotify: func() {
				// Stats notification handled by netstack internally
			},
		})
		util.LogModuleInfo("engine", "Connection handler and callbacks set on netstack")

		// Set up mesh DNS allocator (gateway allocates fakeIPs from local pool)
		meshMgr.DNSAllocator = func(domain string) (net.IP, error) {
			return meshMgr.GetFakeIPPool().Lookup(domain), nil
		}

		meshMgr.Start(ns, p2p.GlobalP2PManager)

		// Configure netstack with mesh manager (Link NICs + RouteSelector)
		ns.SetMeshManager(meshMgr)
		util.LogModuleInfo("engine", "Mesh manager configured on netstack (Link NICs + RouteSelector)")

		util.LogModuleInfo("engine", "Mesh wired to engine (vip=%s allVIPs=%v tunEnabled=%v)", meshVIP, allVIPs, tunEnabled)
	}

	return &TUNResource{engine: engine, netstack: ns}
}

const stoppedMarkerPath = "data/state/stopped"
const lastGoodWorkerPath = "data/state/last-good-worker"

func writeStoppedMarker() {
	_ = os.MkdirAll("data/state", 0755)
	_ = os.WriteFile(stoppedMarkerPath, []byte("1"), 0644)
}

func removeStoppedMarker() {
	_ = os.Remove(stoppedMarkerPath)
}

func wasStoppedGracefully() bool {
	_, err := os.Stat(stoppedMarkerPath)
	return err == nil
}

// writeLastGoodWorker persists the path of the last worker binary that
// successfully started (sent ready signal). Used by watchdog to fall back
// if a new binary fails.
func writeLastGoodWorker(path string) {
	_ = os.MkdirAll("data/state", 0755)
	_ = os.WriteFile(lastGoodWorkerPath, []byte(path), 0644)
}

// readLastGoodWorker reads the persisted last good worker binary path.
// Returns empty string if not found or unreadable.
func readLastGoodWorker() string {
	data, err := os.ReadFile(lastGoodWorkerPath)
	if err != nil {
		return ""
	}
	return string(data)
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
