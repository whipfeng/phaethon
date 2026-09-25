// phaethon - L4 proxy forwarding tool
// Copyright (C) 2026 phaethon authors
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package main

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"phaethon/admin"
	"phaethon/config"
	"phaethon/db"
	"phaethon/dialer"
	"phaethon/mesh"
	"phaethon/p2p"
	"phaethon/pkg/signing"
	"phaethon/reverse"
	"phaethon/server"
	"phaethon/tun"
	"phaethon/util"
)

//go:embed conf/default.yaml
var defaultConfig []byte

// Build info set via -ldflags at build time.
var (
	Version  = "dev"
	Platform = "" // e.g., "linux", "windows", "darwin"
	Arch     = "" // e.g., "amd64", "arm64"
)

// activeRuleConf holds the most recent loaded runtime configuration so callbacks
// scheduled after a reload can still locate the current group instance.
var activeRuleConf atomic.Pointer[config.RuleConfiguration]

type activeResources struct {
	ruleConf          *config.RuleConfiguration
	listeners         []net.Listener
	reverseServers    []*server.ReverseServer
	mappingListeners  map[string]net.Listener       // mapping name -> listener
	mappingReverse    map[string]*server.ReverseServer // mapping name -> reverse server
	healthStop        chan struct{}
	subscriptionStop  chan struct{}
	reverseClientStop chan struct{}                    // global stop for all reverse clients
	reverseClientStops map[string]chan struct{}        // per-config stop channels
	reverseClientWG   sync.WaitGroup
	tunRes            *TUNResource
	meshMgr           *mesh.MeshManager
	adminServer       *admin.AdminServer
	once              sync.Once
}

func (r *activeResources) closeAll() {
	r.once.Do(func() {
		for _, ln := range r.listeners {
			ln.Close()
		}
		for _, rs := range r.reverseServers {
			rs.Close()
		}
		if r.healthStop != nil {
			close(r.healthStop)
		}
		if r.subscriptionStop != nil {
			close(r.subscriptionStop)
		}
		if r.reverseClientStop != nil {
			close(r.reverseClientStop)
			r.reverseClientWG.Wait()
		}
		if r.meshMgr != nil {
			r.meshMgr.Stop()
		}
		if r.tunRes != nil {
			r.tunRes.Stop()
		}
		if r.adminServer != nil {
			r.adminServer.Close()
		}
	})
}

// toggleTUN starts or stops the TUN device on the running stack.
// The gVisor netstack is always running; this only controls the TUN input source.
func toggleTUN(enable bool, res *activeResources) error {
	if res == nil || res.ruleConf == nil {
		return fmt.Errorf("runtime not ready")
	}
	if enable {
		if res.tunRes == nil || res.tunRes.engine == nil {
			return fmt.Errorf("stack not running")
		}
		if res.tunRes.engine.IsTUNRunning() {
			return nil
		}
		return res.tunRes.engine.StartTUN()
	}
	if res.tunRes != nil && res.tunRes.engine != nil {
		return res.tunRes.engine.StopTUN()
	}
	return nil
}

// buildTUNStatus returns the current TUN state for the admin API.
func buildTUNStatus(res *activeResources) map[string]interface{} {
	if res == nil || res.ruleConf == nil {
		return map[string]interface{}{
			"available":     tun.Available(),
			"enabled":       false,
			"bypassGateway": false,
			"dhcpEnabled":   false,
			"running":       false,
			"deviceName":    "",
			"routes": map[string]interface{}{
				"applied":           false,
				"tunIP":             "",
				"defaultIface":      "",
				"defaultIfaceIndex": 0,
				"originalGateway":   "",
				"exclusions":        []string{},
				"splitTunnels":      []string{},
			},
			"logs":       []string{},
			"stats":      tun.TUNStats{},
			"dhcpLeases": []tun.DHCPLease{},
		}
	}
	enabled := res.ruleConf.TUN.IsEnabled()
	status := map[string]interface{}{
		"available":          tun.Available(),
		"enabled":            enabled,
		"bypassGateway":      res.ruleConf.TUN.IsBypassGateway(),
		"dhcpEnabled":        res.ruleConf.TUN.IsDHCPEnabled(),
		"dhcpStaticBindings": res.ruleConf.TUN.DHCPStaticBindings(),
		"running":            false,
		"deviceName":         "",
		"routes": map[string]interface{}{
			"applied":           false,
			"tunIP":             "",
			"defaultIface":      "",
			"defaultIfaceIndex": 0,
			"originalGateway":   "",
			"exclusions":        []string{},
			"splitTunnels":      []string{},
		},
		"logs":       []string{},
		"stats":      tun.TUNStats{},
		"dhcpLeases": []tun.DHCPLease{},
	}
	if res.tunRes != nil && res.tunRes.engine != nil {
		engine := res.tunRes.engine
		status["running"] = engine.IsTUNRunning()
		status["routes"] = engine.RouteSnapshot()
		status["logs"] = engine.Logs()
		status["stats"] = engine.Stats()
		status["dhcpLeases"] = engine.DHCPLeaseSnapshot()
		if engine.IsTUNRunning() {
			status["deviceName"] = "PhaethonTUN"
		}
	} else {
		status["stats"] = tun.TUNStats{}
		status["dhcpLeases"] = []tun.DHCPLease{}
	}
	return status
}

var runCnt int

// subCacheDir is the subscription cache directory, set during getRuleConf.
// It is kept outside the conf/ directory so subscription cache writes never
// mix with user configuration files.
var subCacheDir string

// dataDir is the unified runtime data directory (data/).
// All non-configuration persistent data (reverse-id, bindings, subscription cache)
// is stored here, separate from the static conf/ directory.
var dataDir = filepath.Join(".", "data")
var configPath = "config.yaml" // default, will be updated in getRuleConf()

func run(ruleConf *config.RuleConfiguration, prev *activeResources) (*activeResources, error) {
	if ruleConf == nil {
		return prev, nil
	}

	// Make the current runtime config available to async callbacks.
	activeRuleConf.Store(ruleConf)

	// Preserve admin server across reloads so the web UI stays up
	// and receives the new config reference.
	var prevAdmin *admin.AdminServer
	if prev != nil {
		prevAdmin = prev.adminServer
		prev.adminServer = nil // prevent Close() from shutting it down
		prev.closeAll()
	}

	// Refresh reverse registry
	reverse.Refresh()

	// Recreate control manager on every reload so dynamic resources
	// are re-injected into the new ruleConf. Old sessions/connections
	// are closed → reverse side detects disconnect and reconnects.
	if server.GlobalControlManager != nil {
		server.GlobalControlManager.CloseAll()
	}
	server.GlobalControlManager = server.NewControlManager(ruleConf, dataDir)
	util.Logger.Printf("ControlManager initialized")

	// Initialize P2P manager
	p2pCache, err := p2p.NewBinaryCache(dataDir)
	if err != nil {
		util.Logger.Printf("P2P cache init failed: %v", err)
	} else {
		if Platform == "" || Arch == "" {
			util.Logger.Printf("ERROR: Platform and Arch must be set via -ldflags at build time")
		}
		if err := p2pCache.SeedOwnBinary(Version, Platform, Arch, p2p.DetectBuildTag()); err != nil {
			util.Logger.Printf("P2P seed own binary failed: %v", err)
		}
		p2p.CleanupBackup()
	}
	p2p.GlobalP2PManager = p2p.NewP2PManager("phaethon", Version, p2pCache)
	util.Logger.Printf("P2PManager initialized (version=%s, platform=%s/%s)", Version, Platform, Arch)

	// Initialize mesh overlay network (always enabled)
	// If mesh config is missing, create a default configuration
	if ruleConf.Mesh == nil {
		ruleConf.Mesh = &config.MeshConfig{}
		util.Logger.Printf("Mesh: no config found, using defaults")
	}
	
	var meshMgr *mesh.MeshManager
	{
		// Set the overall mesh network range (e.g., 100.0.0.0/8)
		meshNetworkStr := ruleConf.Mesh.GetNetwork()
		_, meshNetwork, err := net.ParseCIDR(meshNetworkStr)
		if err != nil {
			return nil, fmt.Errorf("mesh network invalid: %w", err)
		}
		if err := mesh.SetMeshCIDR(meshNetworkStr); err != nil {
			return nil, fmt.Errorf("mesh set network fail: %w", err)
		}

		// Determine subnet prefix length:
		// - If config has an explicit subnet, use its prefix length
		// - Otherwise, default to network_prefix + 8 (256 possible subnets)
		networkPrefixLen, _ := meshNetwork.Mask.Size()
		subnetPrefixLen := networkPrefixLen + 8
		if cfgSubnet := ruleConf.Mesh.GetSubnet(); cfgSubnet != "" {
			if _, cfgNet, err := net.ParseCIDR(cfgSubnet); err == nil {
				subnetPrefixLen, _ = cfgNet.Mask.Size()
			}
		}

		// Load mesh state: config.yaml is the source of truth, with migration from mesh-state.json
		// NodeID priority: config → mesh-state.json (migrate) → generate
		// Subnet priority: config → mesh-state.json (migrate) → auto-allocate

		// Try to migrate from mesh-state.json if config doesn't have values
		if ruleConf.Mesh.NodeID == "" || ruleConf.Mesh.GetSubnet() == "" {
			state, err := mesh.LoadState(dataDir)
			if err != nil {
				util.Logger.Printf("WARNING: load mesh-state.json fail: %v", err)
			}
			if state != nil {
				migrated := false
				if ruleConf.Mesh.NodeID == "" && state.NodeID != "" {
					ruleConf.Mesh.NodeID = state.NodeID
					util.Logger.Printf("Mesh: migrated node-id from mesh-state.json to config.yaml: %s", state.NodeID)
					migrated = true
				}
				if ruleConf.Mesh.GetSubnet() == "" && state.Subnet != "" {
					ruleConf.Mesh.Subnet = state.Subnet
					util.Logger.Printf("Mesh: migrated subnet from mesh-state.json to config.yaml: %s", state.Subnet)
					migrated = true
				}
				if migrated {
					// Persist updated config to the database
					if err := db.ImportRuleConf(ruleConf); err != nil {
						util.Logger.Printf("WARNING: persist config after mesh migration fail: %v", err)
					}
					// Remove old mesh-state.json
					stateFile := filepath.Join(dataDir, "state", "mesh-state.json")
					if err := os.Remove(stateFile); err != nil && !os.IsNotExist(err) {
						util.Logger.Printf("WARNING: remove old mesh-state.json fail: %v", err)
					} else {
						util.Logger.Printf("Mesh: removed old mesh-state.json (migrated to config.yaml)")
					}
				}
			}
		}

		// Generate nodeID if still empty
		if ruleConf.Mesh.NodeID == "" {
			ruleConf.Mesh.NodeID, err = mesh.GenerateNodeID()
			if err != nil {
				return nil, fmt.Errorf("mesh generate node id fail: %w", err)
			}
			util.Logger.Printf("Mesh: generated new nodeID: %s", ruleConf.Mesh.NodeID)
		}

		// Auto-allocate subnet if still empty
		if ruleConf.Mesh.GetSubnet() == "" {
			ruleConf.Mesh.Subnet, err = mesh.AllocateSubnet(meshNetwork, subnetPrefixLen, nil)
			if err != nil {
				return nil, fmt.Errorf("mesh allocate subnet fail: %w", err)
			}
			util.Logger.Printf("Mesh: auto-allocated subnet: %s", ruleConf.Mesh.Subnet)
			// Persist updated config with auto-allocated subnet
			if err := db.ImportRuleConf(ruleConf); err != nil {
				util.Logger.Printf("WARNING: persist config after subnet allocation fail: %v", err)
			}
		}

		meshSubnetStr := ruleConf.Mesh.GetSubnet()

		// Parse subnet and derive VIP
		_, meshSubnet, err := net.ParseCIDR(meshSubnetStr)
		if err != nil {
			return nil, fmt.Errorf("mesh subnet invalid: %w", err)
		}
		vip := mesh.DeriveVIPFromSubnet(meshSubnet)

		util.Logger.Printf("[MESH-DEBUG] Before MeshManager creation: nodeID=%s subnet=%s vip=%s", 
			ruleConf.Mesh.NodeID, meshSubnetStr, vip)

		domainSuffixes := ruleConf.Mesh.GetDomainSuffixes()
		advertise := ruleConf.Mesh.GetAdvertise()
		meshMgr = mesh.NewMeshManager(ruleConf.Mesh.NodeID, vip, nil, meshSubnet, meshSubnetStr, domainSuffixes, advertise, meshNetwork, subnetPrefixLen)
		meshMgr.SetDataDir(dataDir)
		
		// Set static IPIP routes
		staticRoutes := ruleConf.Mesh.StaticRoutes
		staticDomainSuffixes := ruleConf.Mesh.GetStaticDomainSuffixes()
		meshMgr.SetStaticRoutes(staticRoutes, staticDomainSuffixes)
		
		mesh.GlobalMeshManager = meshMgr
		p2p.GlobalP2PManager.SetMeshInfo(ruleConf.Mesh.NodeID, vip.String())
		p2p.GlobalP2PManager.SetMeshHandler(meshMgr)
		util.Logger.Printf("Mesh enabled: nodeID=%s vip=%s subnet=%s network=%s subnetPrefix=/%d domainSuffixes=%v advertise=%v", ruleConf.Mesh.NodeID, vip, meshSubnetStr, meshNetworkStr, subnetPrefixLen, domainSuffixes, advertise)
	}

	// Start TUN engine BEFORE P2P peers so mesh has its TUN reference
	// ready when P2P receives the first mesh frames.
	res := &activeResources{
		ruleConf:           ruleConf,
		mappingListeners:   make(map[string]net.Listener),
		mappingReverse:     make(map[string]*server.ReverseServer),
		reverseClientStops: make(map[string]chan struct{}),
	}
	res.tunRes = startEngine(ruleConf, meshMgr, nil)
	if res.tunRes != nil {
		res.meshMgr = meshMgr
	}

	// Start P2P peers:
	// Mesh is always enabled, so ALL compatible proxies (SOCKS5/Trojan/HTunnel) with P2P enabled get P2P automatically
	util.LogInfo("[MAIN] Starting P2P peers, total proxies: %d", len(ruleConf.Proxies))
	for _, proxy := range ruleConf.Proxies {
		util.LogInfo("[MAIN] Checking proxy %s (type=%s, enabled=%v, p2p=%v)", proxy.Name, proxy.Type, proxy.IsEnabled(), proxy.IsP2P())
		if !proxy.IsEnabled() {
			continue
		}
		isCompatible := proxy.Type == "socks5" || proxy.Type == "trojan" || proxy.Type == "h_tunnel"
		if !isCompatible {
			continue
		}
		if !proxy.IsP2P() {
			util.LogInfo("[MAIN] Skipping P2P for proxy %s (p2p disabled)", proxy.Name)
			continue
		}
		util.LogInfo("[MAIN] Starting P2P peer for proxy %s (type=%s)", proxy.Name, proxy.Type)
		go p2p.GlobalP2PManager.StartPeer(proxy)
	}

	// Group trojan mappings by port for SNI routing (non-reverse only)
	trojanPortGroups := make(map[int][]*config.Mapping)
	var otherMappings []*config.Mapping

	for _, m := range ruleConf.Mappings {
		if !m.IsEnabled() {
			continue
		}
		if m.Type == "trojan" && m.ReverseAddress == "" {
			trojanPortGroups[m.Port] = append(trojanPortGroups[m.Port], m)
		} else {
			otherMappings = append(otherMappings, m)
		}
	}

	// 1. Start trojan port groups (SNI routing)
	for port, trojanMappings := range trojanPortGroups {
		if len(trojanMappings) == 1 && trojanMappings[0].Sni == "" {
			// Single trojan, no SNI
			m := trojanMappings[0]
			util.Logger.Printf("Binding mapping: %+v", m)
			ln, err := server.StartTrojan(ruleConf, m)
			if err != nil {
				util.Logger.Printf("ERROR: bind trojan fail: %v", err)
				continue
			}
			res.listeners = append(res.listeners, ln)
		} else {
			// Multiple trojans on same port, use SNI routing
			for _, m := range trojanMappings {
				util.Logger.Printf("Binding mapping(SNI): %+v", m)
			}
			ln, err := server.StartTrojanSNI(ruleConf, trojanMappings, port)
			if err != nil {
				util.Logger.Printf("ERROR: bind trojan-sni fail: %v", err)
				continue
			}
			res.listeners = append(res.listeners, ln)
		}
	}

	// 2. Start other mappings
	for _, m := range otherMappings {
		util.Logger.Printf("Binding mapping: %+v", m)

		if m.ReverseAddress != "" {
			// This mapping uses reverse outbound connections instead of TCP listener
			rs, err := startReverseBinding(ruleConf, m)
			if err != nil {
				util.Logger.Printf("ERROR: bind reverse fail: %v", err)
				continue
			}
			res.reverseServers = append(res.reverseServers, rs)
			res.mappingReverse[m.Name] = rs
			continue
		}

		var ln net.Listener
		var err error
		switch m.Type {
		case "socks5":
			ln, err = server.StartSocks5(ruleConf, m)
		case "direct", "DIRECT":
			ln, err = server.StartDirect(ruleConf, m)
		case "trojan":
			ln, err = server.StartTrojan(ruleConf, m)
		case "h_tunnel":
			ln, err = server.StartHTunnel(ruleConf, m)
		case "http":
			ln, err = server.StartHTTP(ruleConf, m)
		case "https":
			ln, err = server.StartHTTPS(ruleConf, m)
		default:
			util.Logger.Printf("ERROR: unsupported mapping type: %s", m.Type)
			continue
		}

		if err != nil {
			util.Logger.Printf("ERROR: bind %s fail: %v", m.Type, err)
			continue
		}
		res.listeners = append(res.listeners, ln)
		res.mappingListeners[m.Name] = ln
	}

	runCnt++
	util.Logger.Printf("------------------------started ok %d------------------------", runCnt)

	// Start health check for best groups
	res.healthStop = startHealthChecks(ruleConf)

	// Start subscription refresh
	res.subscriptionStop = startSubscriptionRefresh(ruleConf, subCacheDir)

	// Start web admin panel (if configured)
	if ruleConf.Admin != nil && ruleConf.Admin.Enabled {
		if prevAdmin != nil {
			// Reuse existing admin server, just update config reference
			prevAdmin.UpdateConfig(ruleConf)
			res.adminServer = prevAdmin
		} else {
			res.adminServer = admin.NewAdminServer(ruleConf, ruleConf.Admin, defaultConfig)
			if err := res.adminServer.Start(); err != nil {
				return nil, fmt.Errorf("admin server start fail: %w", err)
			}
		}
		// Inject admin handler into TUN engine for direct connection handling
		if res.tunRes != nil {
			res.tunRes.engine.SetAdminHandler(res.adminServer)
			util.Logger.Printf("Admin handler injected into TUN engine for direct connection handling")
		}
	}

	// Start reverse clients for all enabled reverse configs. A single stop channel
	// broadcasts shutdown to every goroutine; the WaitGroup waits for all of them.
	res.reverseClientStop = make(chan struct{})
	for _, rc := range ruleConf.ReverseConfigs {
		if rc == nil || !rc.Enabled {
			continue
		}
		configStop := make(chan struct{})
		res.reverseClientStops[rc.Name] = configStop
		res.reverseClientWG.Add(1)
		go func(rc *config.ReverseConfig) {
			defer res.reverseClientWG.Done()
			startReverseClient(rc, ruleConf, configStop, res.reverseClientStop)
		}(rc)
	}

	return res, nil
}

// startReverseBinding starts a mapping using outbound reverse connections.
// It delegates to server.StartReverseMapping which handles all protocol types.
func startReverseBinding(ruleConf *config.RuleConfiguration, m *config.Mapping) (*server.ReverseServer, error) {
	return server.StartReverseMapping(ruleConf, m)
}

func writeDefaultConfig(path string) error {
	return os.WriteFile(path, defaultConfig, 0644)
}

func getRuleConf() *config.RuleConfiguration {
	workDir, err := os.Getwd()
	if err != nil {
		util.Logger.Printf("ERROR: get working directory fail: %v", err)
		return nil
	}

	// 1. Load .env from working directory (optional).
	envFile := filepath.Join(workDir, ".env")
	if err := config.LoadEnvFile(envFile); err != nil {
		util.Logger.Printf("ERROR: load .env fail: %v", err)
	}

	// 2. Load configuration from the embedded database (single source of
	//    truth). On a fresh database, import the legacy config.yaml — or
	//    generate the embedded default — so existing deployments migrate
	//    transparently.
	configPath = filepath.Join(workDir, "config.yaml")
	empty, err := db.IsEmptyConfig()
	if err != nil {
		util.Logger.Printf("ERROR: inspect db config fail: %v", err)
	}
	if empty {
		imported := false
		if _, statErr := os.Stat(configPath); statErr == nil {
			util.Logger.Printf("database empty, importing existing config from %s", configPath)
			if importErr := db.ImportYAML(configPath); importErr != nil {
				util.Logger.Printf("ERROR: import config into db fail: %v", importErr)
			} else {
				imported = true
			}
		}
		if !imported && len(defaultConfig) > 0 {
			util.Logger.Printf("generating default config as fallback")
			if genErr := writeDefaultConfig(configPath); genErr != nil {
				util.Logger.Printf("ERROR: generate initial config fail: %v", genErr)
			} else if importErr := db.ImportYAML(configPath); importErr != nil {
				util.Logger.Printf("ERROR: import default config into db fail: %v", importErr)
			}
		}
	}

	var ruleConf *config.RuleConfiguration
	conf, err := db.LoadRuleConf()
	if err != nil {
		util.Logger.Printf("ERROR: load config from db fail: %v", err)
	} else {
		ruleConf = conf
	}

	if ruleConf == nil {
		util.Logger.Printf("No valid configuration found!")
		return nil
	}

	// 4. Ensure every instance has a stable ReverseID (even pure registry instances
	//    with no reverse configs need one for identification in the admin UI).
	//    ReverseID is stored in data/setup/reverse-id file.
	_ = loadOrGenerateInstanceReverseID(ruleConf)

	// 5. Normalize reverse configs: assign Seq numbers to any config lacking one.
	normalizeReverseConfigs(ruleConf)

	// 7. Load subscription node pools from the on-disk cache so groups have nodes
	// immediately without blocking startup on network I/O. Missing or stale caches
	// are refreshed in the background once the listeners are up.
	subCacheDir = filepath.Join(dataDir, "cache", "subscription")
	for _, sub := range ruleConf.Subscriptions {
		if !sub.IsEnabled() || sub.URL == "" {
			continue
		}
		cached, err := config.LoadSubscriptionCache(subCacheDir, sub.Name)
		if err != nil || cached == "" {
			continue
		}
		proxies, err := config.ParseSubscription(cached)
		if err != nil {
			util.Logger.Printf("subscription %s cache parse failed: %v", sub.Name, err)
			continue
		}
		sub.SetNodes(proxies)
		for _, g := range ruleConf.ProxyGroups {
			if !g.IsEnabled() || g.Subscription != sub.Name {
				continue
			}
			g.RebuildProxies()
			util.Logger.Printf("group %s (sub=%s): loaded %d nodes from cache, members %d", g.Name, sub.Name, len(sub.SubProxies), len(g.Members))
		}
	}

	// 8. Configure UDP port range for all listeners
	if ruleConf.UDPPortRange != "" {
		parts := strings.SplitN(ruleConf.UDPPortRange, "-", 2)
		if len(parts) == 2 {
			min, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
			max, _ := strconv.Atoi(strings.TrimSpace(parts[1]))
			dialer.SetUDPPortRange(min, max)
			util.Logger.Printf("UDP port range: %d-%d", min, max)
		}
	}

	if ruleConf.NeedHysteria2() {
		util.Logger.Printf("Hysteria2 Available!!!")
	}

	loadHealthState(ruleConf)

	return ruleConf
}

// loadOrGenerateInstanceReverseID returns the instance-level ReverseID.
// Priority: data/setup/reverse-id file > generate new.
// If a new one is generated, it is saved to the reverse-id file.
func loadOrGenerateInstanceReverseID(ruleConf *config.RuleConfiguration) string {
	// Load from data/setup/reverse-id file
	dataDir := filepath.Join("data", "setup")
	id, err := reverse.GetReverseID(dataDir)
	if err != nil {
		util.Logger.Printf("[REVERSE] load/generate instance reverse-id fail: %v", err)
		return ""
	}
	return id
}

// assignSeqToReverseConfigs assigns the next unused integer starting from start+1
// to any config with Seq <= 0, preserving already-used numbers.
func assignSeqToReverseConfigs(configs []*config.ReverseConfig, start int) {
	used := make(map[int]bool)
	for _, rc := range configs {
		if rc != nil && rc.Seq > 0 {
			used[rc.Seq] = true
		}
	}
	next := start + 1
	for _, rc := range configs {
		if rc == nil || rc.Seq > 0 {
			continue
		}
		for used[next] {
			next++
		}
		rc.Seq = next
		used[next] = true
		next++
	}
}

// normalizeReverseConfigs assigns Seq to any rc that lacks one.
func normalizeReverseConfigs(ruleConf *config.RuleConfiguration) {
	if ruleConf == nil {
		return
	}

	// Find max existing Seq to start from.
	maxSeq := 0
	for _, rc := range ruleConf.ReverseConfigs {
		if rc != nil && rc.Seq > maxSeq {
			maxSeq = rc.Seq
		}
	}

	// Assign Seq to configs that lack one.
	assignSeqToReverseConfigs(ruleConf.ReverseConfigs, maxSeq)
}

func main() {
	// Handle --version before anything else: a CLI version query must not
	// spawn a watchdog or worker process (also used by admin package probe).
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Printf("version=%s platform=%s arch=%s\n", Version, Platform, Arch)
		return
	}

	// If PHAETHON_WORKER is not set, this process is the watchdog.
	// The watchdog spawns the actual server as a child and monitors it.
	if os.Getenv("PHAETHON_WORKER") == "" {
		runWatchdogMode()
		os.Exit(0)
	}

	// Extract Java-style -Dkey=value arguments up front so they do not confuse
	// the standard flag parser, while remaining available via util.JavaProp().
	os.Args = util.SetJavaProps(os.Args)

	// Handle --cleanup-pid for self-update: wait for old watchdog, clean up .bak
	handleCleanupPid()

	// Parse command-line flags early so they can override config values.
	// ADMIN_PORT environment variable is also supported for container/script
	// deployments where a flag is less convenient.
	var adminPortFlag string
	flag.StringVar(&adminPortFlag, "admin-port", "", "Override the admin panel port (e.g. 39999). Also reads ADMIN_PORT env var.")
	flag.Parse()

	// Honor ADMIN_PORT env var or -Dadmin.port=xxx when the command-line flag
	// is not provided.
	adminPort := adminPortFlag
	if adminPort == "" {
		adminPort = os.Getenv("ADMIN_PORT")
	}
	if adminPort == "" {
		adminPort = util.JavaProp("admin.port")
	}

	util.Logger.Printf("")
	util.Logger.Printf("phaethon %s", Version)
	util.Logger.Printf("------------------------我是分隔符------------------------~~~")

	// Determine working directory for config loading.
	workDir, err := os.Getwd()
	if err != nil {
		workDir = "."
	}

	util.Logger.Printf("Working directory: %s", workDir)
	util.Logger.Printf("------------------------starting------------------------")

	// Initialize database
	dbPath := filepath.Join(workDir, "phaethon.db")
	
	// Check if this is first run (database doesn't exist)
	_, err = os.Stat(dbPath)
	isFirstRun := os.IsNotExist(err)
	
	// Open database
	if err := db.Init(dbPath); err != nil {
		util.Logger.Printf("ERROR: 初始化数据库失败: %v", err)
		fmt.Fprintf(os.Stderr, "初始化数据库失败: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()
	
	if isFirstRun {
		// Interactive initialization is only for truly fresh deployments.
		// An existing config.yaml means an upgrade from the YAML era: skip
		// the prompts so getRuleConf() imports it transparently.
		legacyConfig := filepath.Join(workDir, "config.yaml")
		if _, err := os.Stat(legacyConfig); err == nil {
			util.Logger.Printf("首次启动（数据库不存在），检测到 config.yaml，将自动导入并跳过交互初始化")
		} else {
			util.Logger.Printf("首次启动，进入初始化模式")
			if err := db.InteractiveInit(); err != nil {
				util.Logger.Printf("ERROR: 交互式初始化失败: %v", err)
				fmt.Fprintf(os.Stderr, "交互式初始化失败: %v\n", err)
				os.Exit(1)
			}
		}
	} else {
		util.Logger.Printf("数据库已加载: %s", dbPath)
	}

	// Load config first
	ruleConf := getRuleConf()

	// Migrate proxy groups: update ManualProxies from config.yaml Proxies field
	if ruleConf != nil {
		if err := db.MigrateProxyGroups(ruleConf); err != nil {
			util.Logger.Printf("WARNING: 迁移代理组失败: %v", err)
		} else {
			util.Logger.Printf("代理组迁移完成")
		}
	}

	// Setup rotating log file (10MB max)
	logPath := filepath.Join(dataDir, "logs", "phaethon.log")
	_ = os.MkdirAll(filepath.Join(dataDir, "logs"), 0755)
	if err := util.SetupLogFile(logPath, 10); err != nil {
		util.Logger.Printf("WARNING: setup log file failed: %v", err)
	} else {
		util.Logger.Printf("Log file: %s (max 10MB, rotates to .old)", logPath)
	}

	// Apply -admin-port / ADMIN_PORT override if provided
	if adminPort != "" && ruleConf != nil {
		if ruleConf.Admin == nil {
			ruleConf.Admin = &config.AdminConfig{Enabled: true}
		}
		// Preserve host, replace port
		host := "127.0.0.1"
		if ruleConf.Admin.Addr != "" {
			if h, _, err := net.SplitHostPort(ruleConf.Admin.Addr); err == nil && h != "" {
				host = h
			}
		}
		ruleConf.Admin.Addr = net.JoinHostPort(host, adminPort)
		ruleConf.Admin.Enabled = true
		util.Logger.Printf("[STARTUP] admin port overridden to %s", ruleConf.Admin.Addr)
	}

	// Start with loaded config
	var resources *activeResources
	startupStart := time.Now()
	util.LogDebug("Config load took %v", time.Since(startupStart))
	resources, err = run(ruleConf, resources)
	if err != nil {
		if strings.Contains(err.Error(), "admin server start fail") {
			adminAddr := "unknown"
			if ruleConf != nil && ruleConf.Admin != nil {
				adminAddr = ruleConf.Admin.Addr
			}
			fmt.Fprintf(os.Stderr, "\nAdmin server failed to start on %s\n", adminAddr)
			fmt.Fprintf(os.Stderr, "Working directory: %s\n", workDir)
			fmt.Fprintf(os.Stderr, "\n管理面板启动失败，地址: %s\n", adminAddr)
			fmt.Fprintf(os.Stderr, "工作目录: %s\n", workDir)
			fmt.Fprintf(os.Stderr, "\nPress Enter to exit... / 按回车键退出...")
			bufio.NewReader(os.Stdin).ReadString('\n')
			os.Exit(1)
		}
		_ = writeStartupError(err)
		util.Logger.Fatalf("startup failed: %v", err)
	}
	util.LogDebug("Total startup took %v", time.Since(startupStart))

	// Wire up admin callbacks
	if resources != nil && resources.adminServer != nil {
		wireAdminCallbacks(resources)
		resources.adminServer.OnReload = func() {
			util.Logger.Printf("------------------------admin reload triggered------------------------")
			if newConf := getRuleConf(); newConf != nil {
				newResources, err := run(newConf, resources)

				if err != nil {
					util.Logger.Printf("reload failed: %v", err)
					return
				}
				resources = newResources
			}
		}
		resources.adminServer.OnTUNToggle = func(enable bool) error {
			return toggleTUN(enable, resources)
		}
		resources.adminServer.GetTUNStatus = func() map[string]interface{} {
			return buildTUNStatus(resources)
		}
		resources.adminServer.OnDHCPStaticBindingsUpdate = func(bindings []config.DHCPStaticBinding) {
			if resources.tunRes != nil && resources.tunRes.engine != nil {
				resources.tunRes.engine.UpdateDHCPStaticBindings(bindings)
			}
		}
	}

	// Interactive mode: open the web reverse wizard in the default browser.
	if ruleConf != nil && ruleConf.Interactive {
		if resources != nil && resources.adminServer != nil {
			openReverseWizardURL(resources.adminServer)
		} else {
			util.Logger.Printf("[INTERACTIVE] admin panel disabled; cannot open reverse wizard")
		}
	}

	// Signal the watchdog that the server is fully initialized.
	if os.Getenv("PHAETHON_WORKER") != "" {
		emitProtocolMsg(map[string]bool{"ready": true})
		startWorkerHeartbeat()
	}

	watchAndRun(resources)
}

// openReverseWizardURL opens the admin reverse wizard page in the user's default browser.
func openReverseWizardURL(adminServer *admin.AdminServer) {
	addr := adminServer.ListenAddr()
	if addr == "" {
		addr = "127.0.0.1:39999"
	}

	// Parse host:port, replacing unspecified addresses with 127.0.0.1 so the
	// browser opens a usable URL instead of [::] or an IPv6 wildcard.
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// addr may already contain a scheme or be a bare port like ":39999"
		if strings.HasPrefix(addr, ":") {
			host, port = "127.0.0.1", addr[1:]
		} else if strings.Contains(addr, "://") {
			url := addr + "/reverse"
			util.Logger.Printf("[INTERACTIVE] opening reverse wizard: %s", url)
			if err := util.OpenBrowser(url); err != nil {
				util.Logger.Printf("[INTERACTIVE] open browser failed: %v", err)
			}
			return
		} else {
			host, port = addr, "39999"
		}
	}
	if host == "" || host == "[::]" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}

	url := fmt.Sprintf("http://%s:%s/reverse", host, port)
	util.Logger.Printf("[INTERACTIVE] opening reverse wizard: %s", url)
	if err := util.OpenBrowser(url); err != nil {
		util.Logger.Printf("[INTERACTIVE] open browser failed: %v", err)
	}
}

// watchAndRun waits for shutdown signals.
// Config changes are now applied immediately in-memory by the admin panel;
// file persistence is for next-start guarantee only.
func watchAndRun(resources *activeResources) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)

	util.Logger.Printf("Server running. Press Ctrl+C to stop.")
	for {
		select {
		case sig := <-sigCh:
			util.Logger.Printf("Received signal %v, shutting down...", sig)
			writeStoppedMarker()
			if resources != nil {
				resources.closeAll()
			}
			return
		case <-consoleCloseNotify():
			util.Logger.Printf("Received console close event, shutting down...")
			writeStoppedMarker()
			if resources != nil {
				resources.closeAll()
			}
			return
		}
	}
}

// childProtocolMsg is a JSON message sent by the worker child on stdout.
// Lines that do not match this schema are forwarded to the watchdog's stdout.
type childProtocolMsg struct {
	Ready     *bool `json:"ready,omitempty"`
	Heartbeat *bool `json:"heartbeat,omitempty"`
}

// childProcess wraps an os.Process with protocol state from the child's stdout.
type childProcess struct {
	proc     *os.Process
	stdout   io.ReadCloser
	ready    atomic.Bool
	lastHB   atomic.Int64 // unix nano of last heartbeat
	done     chan struct{} // closed when stdout reader exits
}

// childReadyTimeout is how long the watchdog waits for the child to signal
// ready before treating it as stuck and restarting.
const childReadyTimeout = 120 * time.Second

// childHeartbeatTimeout is how long the watchdog waits between heartbeats
// before treating the child as stuck.
const childHeartbeatTimeout = 30 * time.Second

// spawnChildProcess starts the worker child with a stdout pipe for protocol
// messages. A goroutine reads lines, parses JSON protocol messages, and
// forwards non-JSON output to the watchdog's stdout.
func spawnChildProcess(exe string) (*childProcess, error) {
	env := buildWorkerEnv()

	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create stdout pipe: %w", err)
	}

	attr := &os.ProcAttr{
		Env: env,
		Files: []*os.File{os.Stdin, stdoutW, os.Stderr},
	}
	proc, err := os.StartProcess(exe, []string{exe}, attr)
	if err != nil {
		stdoutR.Close()
		stdoutW.Close()
		return nil, fmt.Errorf("start worker: %w", err)
	}
	// Close the write end in the parent — only the child writes to it.
	stdoutW.Close()

	cp := &childProcess{
		proc:   proc,
		stdout: stdoutR,
		done:   make(chan struct{}),
	}

	go cp.readProtocol()
	return cp, nil
}

// readProtocol reads lines from the child's stdout. JSON protocol messages
// update the ready/heartbeat state; everything else is forwarded to the
// watchdog's stdout so log output remains visible.
func (cp *childProcess) readProtocol() {
	defer close(cp.done)
	defer cp.stdout.Close()

	scanner := bufio.NewScanner(cp.stdout)
	// Allow up to 64KB per line — log lines can be long.
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) > 0 && line[0] == '{' {
			var msg childProtocolMsg
			if json.Unmarshal(line, &msg) == nil {
				if msg.Ready != nil && *msg.Ready {
					cp.ready.Store(true)
					cp.lastHB.Store(time.Now().UnixNano())
					util.LogInfo("watchdog: child signaled ready")
					continue
				}
				if msg.Heartbeat != nil && *msg.Heartbeat {
					cp.lastHB.Store(time.Now().UnixNano())
					continue
				}
			}
		}
		// Not a protocol message — forward to stdout.
		fmt.Fprintln(os.Stdout, string(line))
	}
}

// kill sends SIGTERM to the child process.
func (cp *childProcess) kill(sig os.Signal) {
	_ = cp.proc.Signal(sig)
}

// wait blocks until the protocol reader finishes (child closed stdout).
func (cp *childProcess) wait() {
	<-cp.done
}

// findLatestPkgAndExtract finds the highest version pkg file in data/packages/
// that matches the current platform/arch, extracts the binary to a temp location,
// and returns the path to the extracted binary along with its version.
// Returns empty strings if no suitable pkg is found.
func findLatestPkgAndExtract() (string, string) {
	packagesDir := "data/packages"

	// List all .pkg files
	entries, err := os.ReadDir(packagesDir)
	if err != nil {
		util.LogDebug("watchdog: cannot read packages dir: %v", err)
		return "", ""
	}

	var latestPkg string
	var latestVersion string

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".pkg") {
			continue
		}

		pkgPath := filepath.Join(packagesDir, entry.Name())

		// Verify and read metadata
		contents, err := signing.VerifyPkg(pkgPath, signing.PublicKey)
		if err != nil {
			util.LogDebug("watchdog: verify pkg %s failed: %v", entry.Name(), err)
			continue
		}

		// Check platform/arch match (using compile-time identifiers, not runtime)
		if contents.Meta.Platform != Platform || contents.Meta.Arch != Arch {
			continue
		}

		// Check if this version is higher
		if latestVersion == "" || p2p.CompareVersions(contents.Meta.Version, latestVersion) > 0 {
			latestVersion = contents.Meta.Version
			latestPkg = pkgPath
		}
	}

	if latestPkg == "" {
		return "", ""
	}

	util.LogInfo("watchdog: found latest pkg: %s (version=%s)", latestPkg, latestVersion)

	// Check if binary already exists in data/worker/
	workerDir := "data/worker"
	binaryPath := filepath.Join(workerDir, fmt.Sprintf("phaethon-%s", latestVersion))
	if _, err := os.Stat(binaryPath); err == nil {
		// Binary already exists, no need to extract
		util.LogInfo("watchdog: binary already exists: %s", binaryPath)
		return binaryPath, latestVersion
	}

	// Extract binary from pkg
	contents, err := signing.VerifyPkg(latestPkg, signing.PublicKey)
	if err != nil {
		util.LogError("watchdog: extract pkg failed: %v", err)
		return "", ""
	}

	// Write binary to data/worker/ directory
	if err := os.MkdirAll(workerDir, 0755); err != nil {
		util.LogError("watchdog: create worker dir failed: %v", err)
		return "", ""
	}
	if err := os.WriteFile(binaryPath, contents.Binary, 0755); err != nil {
		util.LogError("watchdog: write binary failed: %v", err)
		return "", ""
	}

	util.LogInfo("watchdog: extracted binary to %s", binaryPath)
	return binaryPath, latestVersion
}

// selectWorkerBinary picks the worker binary to run: the latest distributed
// pkg only if it is newer than this watchdog build, otherwise the watchdog's
// own executable. This prevents a stale pkg from downgrading a manually
// deployed newer binary, so both upgrade paths (pkg distribution and direct
// binary deployment) stay reliable.
func selectWorkerBinary() string {
	pkgPath, pkgVersion := findLatestPkgAndExtract()
	if pkgPath == "" {
		util.LogInfo("watchdog: no pkg found, using current executable")
	} else if p2p.CompareVersions(pkgVersion, Version) > 0 {
		util.LogInfo("watchdog: pkg version %s newer than self %s, using pkg worker: %s", pkgVersion, Version, pkgPath)
		return pkgPath
	} else {
		util.LogInfo("watchdog: pkg version %s not newer than self %s, using current executable", pkgVersion, Version)
	}
	exe, err := os.Executable()
	if err != nil {
		util.LogError("watchdog: cannot determine executable path: %v", err)
		return ""
	}
	return exe
}

// runWatchdogMode is the watchdog entry point. It spawns the actual server as
// a child process (with PHAETHON_WORKER=1), monitors it via a JSON protocol
// on stdout, and restarts it on crash or stuck detection. On graceful shutdown
// (child writes the stopped marker), the watchdog cleans up and exits.
// SIGTERM/SIGINT received by the watchdog are forwarded to the child.
func runWatchdogMode() {
	util.LogInfo("watchdog: starting in watchdog mode")

	const (
		monitorInterval = 3 * time.Second
		restartCooldown = 10 * time.Second
	)

	// Pick worker binary: distributed pkg if newer than this build, else self
	exe := selectWorkerBinary()
	if exe == "" {
		return
	}
	util.LogInfo("watchdog: using worker binary: %s", exe)

	lastRestart := time.Time{}
	var isRestarting atomic.Bool

	restartChild := func(cp *childProcess, pid int, reason string, alreadyExited bool) *childProcess {
		util.LogInfo("watchdog: %s, restarting child %d", reason, pid)
		isRestarting.Store(true)
		defer func() { isRestarting.Store(false) }()

		if !alreadyExited {
			cp.kill(syscall.SIGTERM)
			// Wait up to 10 seconds for the child to exit. If it's stuck,
			// escalate to SIGKILL.
			select {
			case <-cp.done:
			case <-time.After(10 * time.Second):
				util.LogWarn("watchdog: child %d did not exit after SIGTERM, sending SIGKILL", pid)
				cp.kill(syscall.SIGKILL)
				<-cp.done
			}
			reapChild(pid)
		}

		if elapsed := time.Since(lastRestart); elapsed < restartCooldown {
			remain := restartCooldown - elapsed
			util.LogInfo("watchdog: cooldown, waiting %v", remain.Round(time.Millisecond))
			time.Sleep(remain)
		}
		// Re-scan before restarting (hot swap support): pkg only if newer than self
		newExe := selectWorkerBinary()
		if newExe == "" {
			return nil
		}
		newCp, err := spawnChildProcess(newExe)
		if err != nil {
			util.LogError("watchdog: restart failed: %v", err)
			return nil
		}
		util.LogInfo("watchdog: restarted child (old=%d, new=%d)", pid, newCp.proc.Pid)
		lastRestart = time.Now()
		return newCp
	}

	killResidualWorkers()
	cp, err := spawnChildProcess(exe)
	if err != nil {
		util.LogError("watchdog: initial spawn failed: %v", err)
		return
	}
	util.LogInfo("watchdog: worker started (pid=%d)", cp.proc.Pid)
	spawnTime := time.Now()

	// Forward signals to the child for graceful shutdown.
	// Use a pointer to track current child so signal handler always forwards to the right process.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	currentCp := &cp // pointer to the cp variable
	go func() {
		for sig := range sigCh {
			child := *currentCp
			util.LogInfo("watchdog: received %v, forwarding to child %d", sig, child.proc.Pid)
			// Only write stopped marker if not restarting (to avoid false graceful exit detection)
			if !isRestarting.Load() {
				writeStoppedMarker()
			}
			child.kill(sig)
		}
	}()

	monitorTicker := time.NewTicker(monitorInterval)
	defer monitorTicker.Stop()

	pid := cp.proc.Pid

	util.LogInfo("watchdog: entering monitor loop for pid %d", pid)

	for {
		select {
		case <-cp.done:
			// Child process exited (stdout closed) - reap immediately
			exitCode, reaped := reapChild(pid)
			// Check for self-update request (exit code 42)
			if reaped && exitCode == 42 {
				util.LogInfo("watchdog: child %d requested self-update (exit 42), spawning new watchdog", pid)
				spawnNewWatchdogAndExit(os.Getpid())
				signal.Stop(sigCh)
				return
			}
			if wasStoppedGracefully() {
				util.LogInfo("watchdog: child %d exited gracefully, cleaning up", pid)
				removeStoppedMarker()
				signal.Stop(sigCh)
				return
			}
			// Child exited (hot swap or crash) — restart immediately
			// Pass alreadyExited=true since we already reaped the child
			cp = restartChild(cp, pid, fmt.Sprintf("child %d exited (code=%d)", pid, exitCode), true)
			if cp == nil {
				signal.Stop(sigCh)
				return
			}
			pid = cp.proc.Pid
			spawnTime = time.Now()
			continue
		case <-monitorTicker.C:
			if !processExists(pid) {
				// Child exited but cp.done not yet closed — reap and restart
				exitCode, reaped := reapChild(pid)
				// Check for self-update request (exit code 42)
				if reaped && exitCode == 42 {
					util.LogInfo("watchdog: child %d requested self-update (exit 42), spawning new watchdog", pid)
					spawnNewWatchdogAndExit(os.Getpid())
					signal.Stop(sigCh)
					return
				}
				if wasStoppedGracefully() {
					util.LogInfo("watchdog: child %d exited gracefully, cleaning up", pid)
					removeStoppedMarker()
					signal.Stop(sigCh)
					return
				}
				// Child crashed — restart immediately
				// Pass alreadyExited=true since we already reaped the child
				cp = restartChild(cp, pid, fmt.Sprintf("child %d crashed", pid), true)
				if cp == nil {
					signal.Stop(sigCh)
					return
				}
				pid = cp.proc.Pid
				spawnTime = time.Now()
				continue
			}
			// Check ready timeout: child has not signaled ready within the limit.
			if !cp.ready.Load() && time.Since(spawnTime) > childReadyTimeout {
				// Child still running but stuck — pass alreadyExited=false
				cp = restartChild(cp, pid, fmt.Sprintf("child %d not ready within %v", pid, childReadyTimeout), false)
				if cp == nil {
					signal.Stop(sigCh)
					return
				}
				pid = cp.proc.Pid
				spawnTime = time.Now()
				continue
			}
			// Check heartbeat timeout: child was ready but stopped sending heartbeats.
			if cp.ready.Load() {
				lastHB := time.Unix(0, cp.lastHB.Load())
				if time.Since(lastHB) > childHeartbeatTimeout {
					// Child still running but stuck — pass alreadyExited=false
					cp = restartChild(cp, pid, fmt.Sprintf("child %d heartbeat timeout (%v)", pid, childHeartbeatTimeout), false)
					if cp == nil {
						signal.Stop(sigCh)
						return
					}
					pid = cp.proc.Pid
					spawnTime = time.Now()
				}
			}
		}
	}
}

// spawnNewWatchdogAndExit spawns a new watchdog process and exits the current one.
// The new watchdog receives --cleanup-pid to wait for the old watchdog to exit
// before cleaning up .bak files.
func spawnNewWatchdogAndExit(myPid int) {
	exe, err := os.Executable()
	if err != nil {
		util.LogError("watchdog: self-update: get executable path: %v", err)
		return
	}

	// Build env without PHAETHON_WORKER — new process is a watchdog, not a worker
	env := removeEnvVar(os.Environ(), "PHAETHON_WORKER")

	attr := &os.ProcAttr{
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
		Env:   env,
	}
	args := []string{exe, fmt.Sprintf("--cleanup-pid=%d", myPid)}
	proc, err := os.StartProcess(exe, args, attr)
	if err != nil {
		util.LogError("watchdog: self-update: failed to spawn new watchdog: %v", err)
		return
	}
	util.LogInfo("watchdog: self-update: spawned new watchdog pid=%d, exiting", proc.Pid)
	proc.Release()
	os.Exit(0)
}

// removeEnvVar removes an environment variable from a slice.
func removeEnvVar(env []string, key string) []string {
	prefix := key + "="
	result := make([]string, 0, len(env))
	for _, e := range env {
		if !strings.HasPrefix(e, prefix) {
			result = append(result, e)
		}
	}
	return result
}

// handleCleanupPid checks for --cleanup-pid argument and runs cleanup in background.
func handleCleanupPid() {
	for _, arg := range os.Args[1:] {
		if strings.HasPrefix(arg, "--cleanup-pid=") {
			pidStr := strings.TrimPrefix(arg, "--cleanup-pid=")
			cleanupPid, err := strconv.Atoi(pidStr)
			if err != nil {
				util.LogDebug("cleanup: invalid --cleanup-pid value: %s", pidStr)
				return
			}
			go cleanupAfterUpdate(cleanupPid)
			return
		}
	}
}

// cleanupAfterUpdate waits for the old watchdog to exit, then cleans up .bak files.
func cleanupAfterUpdate(oldWatchdogPid int) {
	util.LogInfo("cleanup: waiting for old watchdog pid=%d to exit", oldWatchdogPid)
	if waitForProcessExit(oldWatchdogPid, 30*time.Second) {
		util.LogInfo("cleanup: old watchdog exited")
	} else {
		util.LogWarn("cleanup: old watchdog pid=%d did not exit within 30s", oldWatchdogPid)
	}

	// Clean up .bak next to the executable
	exe, err := os.Executable()
	if err != nil {
		return
	}
	bakPath := exe + ".bak"
	if err := os.Remove(bakPath); err != nil && !os.IsNotExist(err) {
		util.LogDebug("cleanup: remove %s: %v", bakPath, err)
	} else if err == nil {
		util.LogInfo("cleanup: removed %s", bakPath)
	}

	// Also clean up any .bak from the P2P cache self-update
	p2p.CleanupBackup()
}

// buildWorkerEnv builds the environment for the server child process.
// It inherits the current environment and adds PHAETHON_WORKER=1.
func buildWorkerEnv() []string {
	env := os.Environ()
	env = append(env, "PHAETHON_WORKER=1")
	return env
}

// writeStartupError persists a fatal startup error to disk so the user can
// read it even if the console window closes immediately (e.g. double-click
// on Windows).
func writeStartupError(err error) error {
	path := filepath.Join(dataDir, "logs", "startup-error.log")
	_ = os.MkdirAll(filepath.Join(dataDir, "logs"), 0755)
	msg := fmt.Sprintf("%s startup failed: %v\n", time.Now().Format(time.RFC3339), err)
	return os.WriteFile(path, []byte(msg), 0644)
}

// startReverseClient starts a reverse client goroutine.
// Reuses server.ReverseServer for the data connection pool — no custom relay logic.
// The goroutine exits when stop is closed.
func startReverseClient(rc *config.ReverseConfig, ruleConf *config.RuleConfiguration, configStop <-chan struct{}, globalStop <-chan struct{}) {
	interval := time.Duration(rc.ReconnectInterval) * time.Second
	if interval <= 0 {
		interval = 10 * time.Second
	}

	for {
		select {
		case <-configStop:
			util.Logger.Printf("[REVERSE-CLIENT] stopping reverse client name=%s addr=%s", rc.Name, rc.RegistryProxy)
			return
		case <-globalStop:
			util.Logger.Printf("[REVERSE-CLIENT] stopping reverse client name=%s addr=%s", rc.Name, rc.RegistryProxy)
			return
		default:
		}

		err := runReverseSession(rc, ruleConf, configStop, globalStop)
		if err != nil {
			util.Logger.Printf("[REVERSE-CLIENT] session error (name=%s addr=%s): %v, reconnecting in %v...", rc.Name, rc.RegistryProxy, err, interval)
		} else {
			util.Logger.Printf("[REVERSE-CLIENT] session ended (name=%s addr=%s): %v, reconnecting in %v...", rc.Name, rc.RegistryProxy, err, interval)
		}

		select {
		case <-configStop:
			util.Logger.Printf("[REVERSE-CLIENT] stopping reverse client name=%s addr=%s", rc.Name, rc.RegistryProxy)
			return
		case <-globalStop:
			util.Logger.Printf("[REVERSE-CLIENT] stopping reverse client name=%s addr=%s", rc.Name, rc.RegistryProxy)
			return
		case <-time.After(interval):
		}
	}
}

// publishReverseEvent bumps the reverse topic version so the admin UI knows to
// fetch the latest reverse-connection list via REST.
func publishReverseEvent(rc *config.ReverseConfig) {
	util.DefaultVersionNotifier.BumpVersion("reverse")
}

func runReverseSession(rc *config.ReverseConfig, ruleConf *config.RuleConfiguration, configStop <-chan struct{}, globalStop <-chan struct{}) (err error) {
	// Update runtime state (last error) and notify admin UI whenever this
	// session function returns.
	defer func() {
		if err != nil {
			rc.LastError = err.Error()
		} else {
			rc.LastError = ""
		}
		publishReverseEvent(rc)
	}()

	if rc.RegistryProxy == "" {
		return fmt.Errorf("reverse client requires registry-proxy (must connect to registry through proxy chain)")
	}
	registryProxy := ruleConf.ProxyNames[rc.RegistryProxy]
	if registryProxy == nil {
		return fmt.Errorf("registry proxy not found: %s", rc.RegistryProxy)
	}

	cc := dialer.NewControlClient(registryProxy)
	if err := cc.Connect(); err != nil {
		return fmt.Errorf("control connect fail: %w", err)
	}
	defer cc.Close()

	// If the runtime reloads or shuts down, close the control connection so
	// this session returns immediately instead of staying connected while a
	// new instance registers and gets a different port.
	stopDone := make(chan struct{})
	defer close(stopDone)
	go func() {
		select {
		case <-configStop:
			cc.Close()
		case <-globalStop:
			cc.Close()
		case <-stopDone:
		}
	}()

	// Use the instance-level ReverseID for stable port allocation.
	// All reverse configs from this instance share the same identity;
	// the Seq field differentiates them on the registry.
	clientID := loadOrGenerateInstanceReverseID(ruleConf)

	// For direct listeners configured through the admin wizard, the target may
	// be stored as a single "target-address" string. Split it into host/port so
	// the register request carries the correct destination.
	if rc.DirectDstHost == "" && rc.TargetAddress != "" {
		host, portStr, err := net.SplitHostPort(rc.TargetAddress)
		if err == nil {
			if port, err := strconv.Atoi(portStr); err == nil && port > 0 {
				rc.DirectDstHost = host
				rc.DirectDstPort = port
			}
		}
	}

	req := reverse.ControlRequest{
		Cmd:              "register",
		Name:             rc.Name,
		Seq:              rc.Seq,
		Proto:            registryProxy.Type,
		PreferredPort:    rc.PreferredPort,
		ListenerProto:    rc.ListenerProto,
		ListenerUser:     rc.ListenerUser,
		ListenerPassword: rc.ListenerPassword,
		ListenerSNI:      rc.ListenerSNI,
		DirectDstHost:    rc.DirectDstHost,
		DirectDstPort:    rc.DirectDstPort,
		RegistryProxy:    rc.RegistryProxy,
		ReverseID:        clientID,
	}
	reply, err := cc.Register(req)
	if err != nil {
		return fmt.Errorf("register fail: %w", err)
	}
	if reply.Status != "ok" {
		return fmt.Errorf("register rejected: %s (%s)", reply.Status, reply.Error)
	}

	dynAddr := reply.Address
	actualPort := reply.Port

	// Store the actual port allocated by the registry in runtime state
	// so the admin UI can display the real listening endpoint.
	rc.AssignedPort = actualPort
	rc.LastError = ""
	publishReverseEvent(rc)

	listenerProto := req.ListenerProto
	if listenerProto == "" {
		listenerProto = "socks5"
	}

	// Highlight the actual listening address in the log.
	host := registryProxy.Server
	if host == "" {
		host = rc.RegistryProxy
	}
	util.Logger.Printf("══════════════════════════════════════════")
	util.Logger.Printf("  ✓ 反向客户端注册成功 [%s]", rc.Name)
	util.Logger.Printf("  实际监听地址: %s:%d", host, actualPort)
	util.Logger.Printf("  动态地址:     %s", dynAddr)
	util.Logger.Printf("  监听协议:     %s", listenerProto)
	util.Logger.Printf("══════════════════════════════════════════")
	util.Logger.Printf("[REVERSE-CLIENT] registered: name=%s addr=%s port=%d listener=%s",
		rc.Name, dynAddr, actualPort, listenerProto)

	// Build and output proxy config JSON for easy copy-paste into clients.
	if proxyJSON, err := buildProxyConfigJSON(host, actualPort, listenerProto,
		req.ListenerUser, req.ListenerPassword, req.ListenerSNI); err == nil {
		util.Logger.Printf("  ┌─ 代理配置 JSON ───────────────────────────")
		for _, line := range strings.Split(proxyJSON, "\n") {
			util.Logger.Printf("  │ %s", line)
		}
		util.Logger.Printf("  └───────────────────────────────────────────")
		// Save to file for easy copy-paste into clients. Use a per-config filename
		// so multiple reverse connections do not overwrite each other's JSON.
		proxyFile := filepath.Join(dataDir, "latest-proxy-"+safeFilename(rc.Name)+".json")
		if err := os.WriteFile(proxyFile, []byte(proxyJSON), 0644); err == nil {
			util.Logger.Printf("  代理配置已保存到: %s", proxyFile)
		}
	}

	go cc.Keepalive()
	go cc.StartMonitor()

	// The reverse-side handler must be a real proxy protocol so the registry
	// can carry the fixed destination through the proxy framing. For a direct
	// listener we run a SOCKS5 server on the reverse side; the data
	// connection still uses the original outbound proxy.
	reverseProto := listenerProto
	if listenerProto == "direct" {
		reverseProto = "socks5"
	}

	mapping := &config.Mapping{
		Name:                  "rev-map-" + dynAddr[:8],
		Type:                  reverseProto,
		ReverseAddress:        dynAddr,
		ReverseProxy:          rc.RegistryProxy,
		ReverseMaxConnections: 3,
		ReverseRetryInterval:  5000,
		Username:              req.ListenerUser,
		Password:              req.ListenerPassword,
		Sni:                   req.ListenerSNI,
	}

	rvSrv, err := server.StartReverseMapping(ruleConf, mapping)
	if err != nil {
		return fmt.Errorf("start reverse mapping fail: %w", err)
	}
	defer rvSrv.CloseForce()

	util.Logger.Printf("[REVERSE-CLIENT] data pool running via ReverseServer for %s", dynAddr)

	select {
	case <-cc.Done():
		util.Logger.Printf("[REVERSE-CLIENT] control channel lost, closing session")
	}

	return nil
}

// safeFilename returns a filesystem-safe version of s by replacing anything
// that is not alphanumeric, '-', or '_' with '-'.
func safeFilename(s string) string {
	if s == "" {
		return "default"
	}
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, s)
}

// buildProxyConfigJSON generates a Surge/Shadowrocket-style proxy config JSON.
func buildProxyConfigJSON(host string, port int, proto, user, password, sni string) (string, error) {
	cfg := map[string]interface{}{
		"host":   host,
		"port":   strconv.Itoa(port),
		"type":   strings.Title(proto),
		"udp":    1,
		"obfs":   "none",
		"plugin": "none",
	}

	switch proto {
	case "socks5":
		cfg["method"] = "auto"
		if user != "" {
			cfg["user"] = user
		}
		if password != "" {
			cfg["password"] = password
		}
	case "http":
		if user != "" {
			cfg["user"] = user
		}
		if password != "" {
			cfg["password"] = password
		}
	case "trojan":
		if password != "" {
			cfg["password"] = password
		}
		if sni != "" {
			cfg["peer"] = sni
		}
		cfg["allowInsecure"] = 1
	case "https":
		if user != "" {
			cfg["user"] = user
		}
		if password != "" {
			cfg["password"] = password
		}
		if sni != "" {
			cfg["peer"] = sni
		}
		cfg["allowInsecure"] = 1
	case "h_tunnel":
		if password != "" {
			cfg["password"] = password
		}
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// wireAdminCallbacks connects admin-server actions to the current runtime config.
func wireAdminCallbacks(resources *activeResources) {
	if resources == nil || resources.adminServer == nil || resources.ruleConf == nil {
		return
	}
	adminSrv := resources.adminServer
	adminSrv.RefreshSubscription = func(subName string) error {
		conf := adminSrv.GetConfig()
		if conf == nil {
			return fmt.Errorf("config not ready")
		}
		for _, sub := range conf.Subscriptions {
			if sub.Name == subName {
				return refreshSubscription(conf, sub, subCacheDir)
			}
		}
		return fmt.Errorf("subscription not found: %s", subName)
	}
	adminSrv.CheckGroupHealth = func(groupName string) error {
		conf := adminSrv.GetConfig()
		if conf == nil {
			return fmt.Errorf("config not ready")
		}
		for _, g := range conf.ProxyGroups {
			if g.Name == groupName {
				checkGroupHealth(conf, g)
				return nil
			}
		}
		return fmt.Errorf("group not found: %s", groupName)
	}
	adminSrv.CheckGroupTest = func(groupName string) error {
		conf := adminSrv.GetConfig()
		if conf == nil {
			return fmt.Errorf("config not ready")
		}
		for _, g := range conf.ProxyGroups {
			if g.Name == groupName {
				checkGroupTest(conf, g)
				return nil
			}
		}
		return fmt.Errorf("group not found: %s", groupName)
	}
	adminSrv.CheckGroupProxyHealth = func(groupName, proxyName string) (config.HealthInfo, error) {
		conf := adminSrv.GetConfig()
		if conf == nil {
			return config.HealthInfo{}, fmt.Errorf("config not ready")
		}
		for _, g := range conf.ProxyGroups {
			if g.Name != groupName {
				continue
			}
			m := config.GroupMember{Name: proxyName, FromSubscription: true}
			for _, mn := range g.ManualProxies {
				if mn == proxyName {
					m.FromSubscription = false
					break
				}
			}
			key := m.HealthKey()
			if !m.FromSubscription {
				if innerGroup, ok := conf.GroupNames[proxyName]; ok {
					selected := innerGroup.NextWithVisited(make(map[string]bool))
					g.SetHealthImmediate(key, selected != nil, 0)
					saveHealthState(conf)
					return g.GetHealthInfo(key), nil
				}
			}
			p := g.Resolve(proxyName)
			if p == nil {
				g.SetHealthImmediate(key, false, 0)
				saveHealthState(conf)
				return g.GetHealthInfo(key), nil
			}
			if strings.EqualFold(p.Type, config.ProxyDIRECT) || strings.EqualFold(p.Type, config.ProxyREJECT) {
				g.SetHealthImmediate(key, true, 0)
				saveHealthState(conf)
				return g.GetHealthInfo(key), nil
			}
			testURL := g.HealthCheckURL
			var alive bool
			var latency time.Duration
			if testURL == "" {
				alive, latency = checkProxyTCPHealth(p)
			} else {
				host, port, useHTTP := parseTestURL(testURL)
				path := getTestPath(testURL)
				alive, latency = checkProxyHealth(p, host, port, useHTTP, path)
			}
			g.SetHealthImmediate(key, alive, latency)
			saveHealthState(conf)
			return g.GetHealthInfo(key), nil
		}
		return config.HealthInfo{}, fmt.Errorf("group not found: %s", groupName)
	}
	adminSrv.CheckSubscriptionHealth = func(subName, nodeName, url string) (config.HealthInfo, error) {
		conf := adminSrv.GetConfig()
		if conf == nil {
			return config.HealthInfo{}, fmt.Errorf("config not ready")
		}
		var sub *config.Subscription
		for _, s := range conf.Subscriptions {
			if s.Name == subName {
				sub = s
				break
			}
		}
		if sub == nil {
			return config.HealthInfo{}, fmt.Errorf("subscription not found: %s", subName)
		}
		sub.SubMu.RLock()
		p := sub.SubProxies[nodeName]
		sub.SubMu.RUnlock()
		if p == nil {
			return config.HealthInfo{}, fmt.Errorf("node not found: %s", nodeName)
		}
		if url == "" {
			url = "http://www.gstatic.com/generate_204"
		}
		host, port, useHTTP := parseTestURL(url)
		path := getTestPath(url)
		alive, latency := checkProxyHealth(p, host, port, useHTTP, path)

		// Propagate the result to every group that references this subscription
		// so the proxy-group node popup can show latency without running its
		// own health check.
		key := config.GroupMember{Name: nodeName, FromSubscription: true}.HealthKey()
		conf.RLock()
		for _, g := range conf.ProxyGroups {
			if g.Subscription != subName {
				continue
			}
			g.SetHealthImmediate(key, alive, latency)
		}
		conf.RUnlock()
		saveHealthState(conf)

		return config.HealthInfo{
			Alive:     alive,
			Latency:   latency,
			LastCheck: time.Now(),
		}, nil
	}
	adminSrv.CheckProxyHealth = func(proxyName string) (config.HealthInfo, error) {
		conf := adminSrv.GetConfig()
		if conf == nil {
			return config.HealthInfo{}, fmt.Errorf("config not ready")
		}
		p := conf.ProxyNames[proxyName]
		if p == nil {
			return config.HealthInfo{}, fmt.Errorf("proxy not found: %s", proxyName)
		}
		if strings.EqualFold(p.Type, config.ProxyDIRECT) || strings.EqualFold(p.Type, config.ProxyREJECT) {
			return config.HealthInfo{Alive: true, LastCheck: time.Now()}, nil
		}

		// If health-check-url is configured, do a deep health check through the proxy
		if p.HealthCheckURL != "" {
			host, port, useHTTP := parseTestURL(p.HealthCheckURL)
			path := getTestPath(p.HealthCheckURL)
			alive, latency := checkProxyHealth(p, host, port, useHTTP, path)
			return config.HealthInfo{
				Alive:     alive,
				Latency:   latency,
				LastCheck: time.Now(),
			}, nil
		}

		// Otherwise, just test TCP connectivity to the proxy server
		alive, latency := checkProxyTCPHealth(p)
		return config.HealthInfo{
			Alive:     alive,
			Latency:   latency,
			LastCheck: time.Now(),
		}, nil
	}
	adminSrv.GetReverseBindings = func() []server.PortBinding {
		if server.GlobalControlManager == nil {
			return nil
		}
		return server.GlobalControlManager.GetBindings()
	}
	adminSrv.ForceRemoveBinding = func(reverseID string, seq int) error {
		if server.GlobalControlManager == nil {
			return fmt.Errorf("control manager not available")
		}
		return server.GlobalControlManager.ForceRemoveBinding(reverseID, seq)
	}
	adminSrv.OnIncrementalUpdate = func() error {
		// mergeAndInitLocked already updated s.conf in place, which is the same
		// object as resources.ruleConf. All servers see changes immediately.
		activeRuleConf.Store(resources.ruleConf)

		// Sync P2P peers with proxy config changes
		resources.ruleConf.Lock()
		proxies := make([]*config.Proxy, len(resources.ruleConf.Proxies))
		copy(proxies, resources.ruleConf.Proxies)
		resources.ruleConf.Unlock()

		// Build set of existing P2P peer IDs
		existingPeers := make(map[string]bool)
		for _, p := range p2p.GlobalP2PManager.GetPeers() {
			existingPeers[p.ID] = true
		}

		for _, proxy := range proxies {
			isCompatible := proxy.Type == "socks5" || proxy.Type == "trojan" || proxy.Type == "h_tunnel"
			if !isCompatible {
				continue
			}

			_, exists := existingPeers[proxy.Name]

			if proxy.IsEnabled() && !exists {
				// Proxy enabled and not running - start P2P
				util.LogInfo("[P2P] starting peer for newly enabled proxy %s", proxy.Name)
				go p2p.GlobalP2PManager.StartPeer(proxy)
			} else if !proxy.IsEnabled() && exists {
				// Proxy disabled and running - stop P2P
				util.LogInfo("[P2P] stopping peer for disabled proxy %s", proxy.Name)
				p2p.GlobalP2PManager.StopPeer(proxy.Name)
			} else if proxy.IsEnabled() && exists {
				// Proxy config may have changed - check and restart if needed
				oldProxy := p2p.GlobalP2PManager.GetPeerProxy(proxy.Name)
				if oldProxy != proxy {
					// Pointer changed means config was updated
					util.LogInfo("[P2P] restarting peer for proxy %s due to config update", proxy.Name)
					p2p.GlobalP2PManager.RestartPeer(proxy)
				}
			}
		}

		return nil
	}
	adminSrv.OnMappingUpdate = func(old, newMapping *config.Mapping) error {
		// Mapping deleted
		if newMapping == nil && old != nil {
			if ln, ok := resources.mappingListeners[old.Name]; ok {
				ln.Close()
				delete(resources.mappingListeners, old.Name)
			}
			if rs, ok := resources.mappingReverse[old.Name]; ok {
				rs.Close()
				delete(resources.mappingReverse, old.Name)
			}
			// Remove from ruleConf
			resources.ruleConf.Lock()
			for i, m := range resources.ruleConf.Mappings {
				if m.Name == old.Name {
					resources.ruleConf.Mappings = append(resources.ruleConf.Mappings[:i], resources.ruleConf.Mappings[i+1:]...)
					break
				}
			}
			resources.ruleConf.Unlock()
			return nil
		}
		// Mapping added or updated
		if newMapping != nil {
			// Close old listener if exists
			if old != nil {
				if ln, ok := resources.mappingListeners[old.Name]; ok {
					ln.Close()
					delete(resources.mappingListeners, old.Name)
				}
				if rs, ok := resources.mappingReverse[old.Name]; ok {
					rs.Close()
					delete(resources.mappingReverse, old.Name)
				}
			}
			// Start new listener
			if newMapping.IsEnabled() {
				if newMapping.ReverseAddress != "" {
					rs, err := startReverseBinding(resources.ruleConf, newMapping)
					if err != nil {
						util.Logger.Printf("ERROR: bind reverse fail for %s: %v", newMapping.Name, err)
					} else {
						resources.reverseServers = append(resources.reverseServers, rs)
						resources.mappingReverse[newMapping.Name] = rs
					}
				} else {
					var ln net.Listener
					var err error
					switch newMapping.Type {
					case "socks5":
						ln, err = server.StartSocks5(resources.ruleConf, newMapping)
					case "direct", "DIRECT":
						ln, err = server.StartDirect(resources.ruleConf, newMapping)
					case "trojan":
						ln, err = server.StartTrojan(resources.ruleConf, newMapping)
					case "h_tunnel":
						ln, err = server.StartHTunnel(resources.ruleConf, newMapping)
					case "http":
						ln, err = server.StartHTTP(resources.ruleConf, newMapping)
					case "https":
						ln, err = server.StartHTTPS(resources.ruleConf, newMapping)
					default:
						err = fmt.Errorf("unsupported mapping type: %s", newMapping.Type)
					}
					if err != nil {
						util.Logger.Printf("ERROR: bind %s fail for %s: %v", newMapping.Type, newMapping.Name, err)
					} else {
						resources.listeners = append(resources.listeners, ln)
						resources.mappingListeners[newMapping.Name] = ln
					}
				}
			}
		}
		return nil
	}
	adminSrv.OnReverseConfigUpdate = func(old, newConfig *config.ReverseConfig) error {
		// Reverse config deleted
		if newConfig == nil && old != nil {
			// Close the per-config stop channel to stop the goroutine
			if stopCh, ok := resources.reverseClientStops[old.Name]; ok {
				close(stopCh)
				delete(resources.reverseClientStops, old.Name)
			}
			// Remove from ruleConf
			resources.ruleConf.Lock()
			for i, rc := range resources.ruleConf.ReverseConfigs {
				if rc.Name == old.Name {
					resources.ruleConf.ReverseConfigs = append(resources.ruleConf.ReverseConfigs[:i], resources.ruleConf.ReverseConfigs[i+1:]...)
					break
				}
			}
			resources.ruleConf.Unlock()
			return nil
		}
		// Reverse config added or updated
		if newConfig != nil {
			// Stop old goroutine if exists
			if old != nil {
				if stopCh, ok := resources.reverseClientStops[old.Name]; ok {
					close(stopCh)
					delete(resources.reverseClientStops, old.Name)
				}
			}
			// Start new goroutine if enabled
			if newConfig.Enabled {
				stopCh := make(chan struct{})
				resources.reverseClientStops[newConfig.Name] = stopCh
				go startReverseClient(newConfig, resources.ruleConf, stopCh, resources.reverseClientStop)
			}
		}
		return nil
		}

	// Mesh package distribution setup
	if resources.meshMgr != nil && resources.tunRes != nil && resources.tunRes.engine != nil {
		// Set peer lister
		adminSrv.SetPeerLister(func() []admin.PeerBrief {
			peers := resources.meshMgr.GetPeers()
			result := make([]admin.PeerBrief, 0, len(peers))
			for _, p := range peers {
				result = append(result, admin.PeerBrief{
					NodeID: p.NodeID,
				})
			}
			return result
		})

		// Set mesh dial function and DNS resolver
		adminSrv.SetMeshDialFn(
			func(network, addr string) (net.Conn, error) {
				return resources.tunRes.engine.NetDial(network, addr)
			},
			func(domain string) (net.IP, error) {
				return resources.tunRes.engine.ResolveDomain(domain)
			},
		)

		// Set admin port
		if adminAddr := adminSrv.ListenAddr(); adminAddr != "" {
			if _, portStr, err := net.SplitHostPort(adminAddr); err == nil {
				if port, err := strconv.Atoi(portStr); err == nil {
					adminSrv.SetAdminPort(port)
				}
			}
		}

		// Set current version getter for hot swap
		adminSrv.GetCurrentVersion = func() string {
			return Version
		}
		// Set platform/arch getters for hot swap (compile-time identifiers)
		adminSrv.GetPlatform = func() string {
			return Platform
		}
		adminSrv.GetArch = func() string {
			return Arch
		}

		// Set peer registered callback for package sync
		resources.meshMgr.OnPeerRegistered = func(nodeID string) {
			adminSrv.SyncFromPeer(nodeID)
		}

		// Periodic sync as fallback (every 5 minutes)
		go func() {
			ticker := time.NewTicker(5 * time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					for _, peer := range resources.meshMgr.GetPeers() {
						adminSrv.SyncFromPeer(peer.NodeID)
					}
				case <-resources.meshMgr.CloseCh():
					return
				}
			}
		}()
	}
}

// ========== Health Check for best groups ==========

func startHealthChecks(ruleConf *config.RuleConfiguration) chan struct{} {
	stop := make(chan struct{})

	// All groups that have interval configured will be health-checked
	var checkGroups []*config.ProxyGroup
	for _, g := range ruleConf.ProxyGroups {
		if !g.IsEnabled() {
			continue
		}
		if g.HealthCheckInterval != nil && *g.HealthCheckInterval > 0 {
			checkGroups = append(checkGroups, g)
		}
	}
	if len(checkGroups) == 0 {
		return stop
	}

	// Find minimum interval across all checked groups
	minInterval := *checkGroups[0].HealthCheckInterval
	for _, g := range checkGroups[1:] {
		if *g.HealthCheckInterval < minInterval {
			minInterval = *g.HealthCheckInterval
		}
	}

	go func() {
		// Delay initial check to avoid blocking startup
		select {
		case <-time.After(5 * time.Second):
			for _, g := range checkGroups {
				checkGroupHealth(ruleConf, g)
			}
		case <-stop:
			return
		}

		ticker := time.NewTicker(time.Duration(minInterval) * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				for _, g := range checkGroups {
					checkGroupHealth(ruleConf, g)
				}
			case <-stop:
				return
			}
		}
	}()

	return stop
}

const (
	healthCheckConcurrency = 8
	healthCheckTimeout     = 5 * time.Second
)

func checkGroupHealth(ruleConf *config.RuleConfiguration, g *config.ProxyGroup) {
	testURL := g.HealthCheckURL
	var host string
	var port int
	var useHTTP bool
	var path string
	if testURL != "" {
		host, port, useHTTP = parseTestURL(testURL)
		path = getTestPath(testURL)
	}

	var members []config.GroupMember
	members = g.GetMembers()

	type item struct {
		key string
		p   *config.Proxy
	}
	var items []item

	for _, m := range members {
		key := m.HealthKey()
		// Nested group references only come from manual members.
		if m.IsGroup {
			ruleConf.RLock()
			innerGroup, ok := ruleConf.GroupNames[m.Name]
			ruleConf.RUnlock()
			if !ok {
				g.SetHealth(key, false, 0)
				continue
			}
			// Select a member from the nested group to test
			selected := innerGroup.NextWithVisited(make(map[string]bool))
			if selected == nil {
				g.SetHealth(key, false, 0)
				continue
			}
			if strings.EqualFold(selected.Type, config.ProxyDIRECT) || strings.EqualFold(selected.Type, config.ProxyREJECT) {
				g.SetHealth(key, true, 0)
				continue
			}
			// Add the selected proxy to items for actual health check
			items = append(items, item{key: key, p: selected})
			continue
		}

		p := g.ResolveMember(m)
		if p == nil {
			g.SetHealth(key, false, 0)
			continue
		}
		if strings.EqualFold(p.Type, config.ProxyDIRECT) || strings.EqualFold(p.Type, config.ProxyREJECT) {
			g.SetHealth(key, true, 0)
			continue
		}

		items = append(items, item{key: key, p: p})
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, healthCheckConcurrency)
	for _, it := range items {
		wg.Add(1)
		sem <- struct{}{}
		go func(it item) {
			defer wg.Done()
			defer func() { <-sem }()
			var alive bool
			var latency time.Duration
			if testURL == "" {
				// TCP-only check: dial through p.Next to reach p's server
				alive, latency = checkProxyTCPHealth(it.p)
			} else {
				// Deep check: dial through proxy to test URL
				alive, latency = checkProxyHealth(it.p, host, port, useHTTP, path)
			}
			g.SetHealth(it.key, alive, latency)
		}(it)
	}
	wg.Wait()

	saveHealthState(ruleConf)
	util.DefaultVersionNotifier.BumpVersion("stats")
}

// checkGroupTest runs an immediate one-off test for every member of a group.
// Unlike the periodic checkGroupHealth, this uses SetHealthImmediate so the
// result is visible right away.
func checkGroupTest(ruleConf *config.RuleConfiguration, g *config.ProxyGroup) {
	testURL := g.HealthCheckURL
	var host string
	var port int
	var useHTTP bool
	var path string
	if testURL != "" {
		host, port, useHTTP = parseTestURL(testURL)
		path = getTestPath(testURL)
	}

	members := g.GetMembers()
	var wg sync.WaitGroup
	sem := make(chan struct{}, healthCheckConcurrency)
	for _, m := range members {
		key := m.HealthKey()
		var p *config.Proxy
		if m.IsGroup {
			ruleConf.RLock()
			innerGroup, ok := ruleConf.GroupNames[m.Name]
			ruleConf.RUnlock()
			if !ok {
				g.SetHealthImmediate(key, false, 0)
				continue
			}
			p = innerGroup.NextWithVisited(make(map[string]bool))
			if p == nil {
				g.SetHealthImmediate(key, false, 0)
				continue
			}
		} else {
			p = g.ResolveMember(m)
		}

		if p == nil {
			g.SetHealthImmediate(key, false, 0)
			continue
		}
		if strings.EqualFold(p.Type, config.ProxyDIRECT) || strings.EqualFold(p.Type, config.ProxyREJECT) {
			g.SetHealthImmediate(key, true, 0)
			continue
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(key string, p *config.Proxy) {
			defer wg.Done()
			defer func() { <-sem }()
			var alive bool
			var latency time.Duration
			if testURL == "" {
				alive, latency = checkProxyTCPHealth(p)
			} else {
				alive, latency = checkProxyHealth(p, host, port, useHTTP, path)
			}
			g.SetHealthImmediate(key, alive, latency)
		}(key, p)
	}
	wg.Wait()

	saveHealthState(ruleConf)
}

type persistedHealthEntry struct {
	Alive     bool      `json:"alive"`
	LatencyMs int64     `json:"latencyMs"`
	LastCheck time.Time `json:"lastCheck"`
	FailCount int       `json:"failCount"`
}

func healthStatePath() string {
	return filepath.Join(dataDir, "state", "health-state.json")
}

// loadHealthState restores group health from the last saved state so that
// previously tested latency/alive information survives a process restart.
func loadHealthState(ruleConf *config.RuleConfiguration) {
	data, err := os.ReadFile(healthStatePath())
	if err != nil {
		if !os.IsNotExist(err) {
			util.LogWarn("[HEALTH] load state fail: %v", err)
		}
		return
	}
	var state map[string]map[string]persistedHealthEntry
	if err := json.Unmarshal(data, &state); err != nil {
		util.LogWarn("[HEALTH] parse state fail: %v", err)
		return
	}
	loaded := 0
	for _, g := range ruleConf.ProxyGroups {
		if !g.IsEnabled() {
			continue
		}
		groupState, ok := state[g.Name]
		if !ok {
			continue
		}
		for _, m := range g.Members {
			key := m.HealthKey()
			if e, ok := groupState[key]; ok {
				if !e.Alive {
					continue
				}
				g.SetHealth(key, e.Alive, time.Duration(e.LatencyMs)*time.Millisecond)
				loaded++
			}
		}
	}
	util.LogInfo("[HEALTH] loaded %d entries from %d groups", loaded, len(state))
}

// saveHealthState snapshots the current group health to disk.
func saveHealthState(ruleConf *config.RuleConfiguration) {
	ruleConf.RLock()
	defer ruleConf.RUnlock()

	state := make(map[string]map[string]persistedHealthEntry)
	for _, g := range ruleConf.ProxyGroups {
		if !g.IsEnabled() {
			continue
		}
		snap := g.HealthSnapshot()
		if len(snap) == 0 {
			continue
		}
		groupState := make(map[string]persistedHealthEntry, len(snap))
		for key, hi := range snap {
			if hi.LastCheck.IsZero() {
				continue
			}
			groupState[key] = persistedHealthEntry{
				Alive:     hi.Alive,
				LatencyMs: hi.Latency.Milliseconds(),
				LastCheck: hi.LastCheck,
				FailCount: hi.FailCount,
			}
		}
		if len(groupState) > 0 {
			state[g.Name] = groupState
		}
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		util.LogWarn("[HEALTH] marshal state fail: %v", err)
		return
	}

	path := healthStatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		util.LogWarn("[HEALTH] create state dir fail: %v", err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		util.LogWarn("[HEALTH] write state tmp fail: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		util.LogWarn("[HEALTH] rename state fail: %v", err)
	}
}

// checkProxyTCPHealth tests TCP connectivity to the proxy's own server.
func checkProxyTCPHealth(p *config.Proxy) (bool, time.Duration) {
	type result struct {
		alive   bool
		latency time.Duration
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		conn, err := dialer.DialToProxy(p)
		if err != nil {
			done <- result{false, 0}
			return
		}
		conn.Close()
		done <- result{true, time.Since(start)}
	}()

	select {
	case r := <-done:
		return r.alive, r.latency
	case <-time.After(healthCheckTimeout):
		return false, 0
	}
}

func checkProxyHealth(p *config.Proxy, host string, port int, useHTTP bool, path string) (bool, time.Duration) {
	type result struct {
		alive   bool
		latency time.Duration
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		conn, err := dialer.ChainDial(p, host, port)
		if err != nil {
			done <- result{false, 0}
			return
		}
		latency := time.Since(start)
		alive := true

		// If HTTP URL, verify with a simple HEAD request.
		if useHTTP {
			conn.SetDeadline(time.Now().Add(5 * time.Second))
			_, _ = fmt.Fprintf(conn, "HEAD %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", path, host)

			buf := make([]byte, 512)
			n, err := conn.Read(buf)
			if (err != nil && err != io.EOF) || n < 12 {
				alive = false
			} else {
				// Check status code starts with 2 or 3.
				if buf[9] != '2' && buf[9] != '3' {
					alive = false
				}
			}
		}

		conn.Close()
		done <- result{alive, latency}
	}()

	select {
	case r := <-done:
		return r.alive, r.latency
	case <-time.After(healthCheckTimeout):
		return false, 0
	}
}

func parseTestURL(url string) (host string, port int, useHTTP bool) {
	if strings.HasPrefix(url, "http://") {
		useHTTP = true
		url = strings.TrimPrefix(url, "http://")
	} else if strings.HasPrefix(url, "https://") {
		useHTTP = true
		port = 443
		url = strings.TrimPrefix(url, "https://")
	}

	// Remove path
	if idx := strings.Index(url, "/"); idx != -1 {
		url = url[:idx]
	}

	// Parse host:port using net.SplitHostPort for proper IPv6 support
	if h, p, err := net.SplitHostPort(url); err == nil {
		host = h
		port, _ = strconv.Atoi(p)
		return host, port, useHTTP
	}
	host = url
	if port == 0 {
		port = 80
	}
	return host, port, useHTTP
}

func getTestPath(url string) string {
	if idx := strings.Index(url, "://"); idx != -1 {
		url = url[idx+3:]
	}
	if idx := strings.Index(url, "/"); idx != -1 {
		return url[idx:]
	}
	return "/"
}

// ========== Subscription Refresh ==========

func startSubscriptionRefresh(ruleConf *config.RuleConfiguration, subCacheDir string) chan struct{} {
	stop := make(chan struct{})

	var allSubs []*config.Subscription
	var refreshSubs []*config.Subscription
	for _, sub := range ruleConf.Subscriptions {
		if !sub.IsEnabled() {
			continue
		}
		if sub.URL != "" {
			allSubs = append(allSubs, sub)
			if sub.Interval != nil && *sub.Interval > 0 {
				refreshSubs = append(refreshSubs, sub)
			}
		}
	}
	if len(allSubs) == 0 {
		return stop
	}

	// Find minimum interval for periodic refresh.
	var minInterval int
	if len(refreshSubs) > 0 {
		minInterval = *refreshSubs[0].Interval
		for _, sub := range refreshSubs[1:] {
			if *sub.Interval < minInterval {
				minInterval = *sub.Interval
			}
		}
	}

	go func() {
		// Initial refresh from network now that listeners are up. This replaces
		// any stale cached nodes loaded during startup without blocking it.
		var wg sync.WaitGroup
		for _, sub := range allSubs {
			wg.Add(1)
			go func(sub *config.Subscription) {
				defer wg.Done()
				refreshSubscription(ruleConf, sub, subCacheDir)
			}(sub)
		}
		wg.Wait()

		if len(refreshSubs) == 0 {
			return
		}
		ticker := time.NewTicker(time.Duration(minInterval) * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				for _, sub := range refreshSubs {
					refreshSubscription(ruleConf, sub, subCacheDir)
				}
			case <-stop:
				return
			}
		}
	}()

	return stop
}

func refreshSubscription(ruleConf *config.RuleConfiguration, sub *config.Subscription, subCacheDir string) error {
	// Resolve proxy for subscription URL via rules
	subHost, subPort, _ := parseTestURL(sub.URL)
	req := config.NewConnectRequest("tcp", subHost, subPort)
	proxy, _ := ruleConf.Match(req, nil)

	var content string
	var err error
	var fromCache bool
	if proxy != nil && proxy.Type != config.ProxyDIRECT && proxy.Type != config.ProxyREJECT {
		dialFunc := func(network, addr string) (net.Conn, error) {
			host, portStr, _ := net.SplitHostPort(addr)
			port, _ := strconv.Atoi(portStr)
			return dialer.ChainDial(proxy, host, port)
		}
		content, fromCache, err = config.FetchSubscriptionCached(sub.URL, dialFunc, subCacheDir, sub.Name)
	} else {
		content, fromCache, err = config.FetchSubscriptionCached(sub.URL, nil, subCacheDir, sub.Name)
	}
	if fromCache {
		util.Logger.Printf("subscription %s refresh: network fetch failed, using cached copy", sub.Name)
	}
	if err != nil {
		util.Logger.Printf("subscription %s refresh failed: %v", sub.Name, err)
		return fmt.Errorf("fetch subscription fail: %w", err)
	}

	proxies, err := config.ParseSubscription(content)
	if err != nil {
		util.Logger.Printf("subscription %s parse failed: %v", sub.Name, err)
		return fmt.Errorf("parse subscription fail: %w", err)
	}

	// Snapshot old active members per referencing group so they can be
	// restored when the node still exists in the refreshed pool.
	type groupActive struct {
		g      *config.ProxyGroup
		active string
	}
	var groups []groupActive
	for _, g := range ruleConf.ProxyGroups {
		if g.Subscription != sub.Name {
			continue
		}
		groups = append(groups, groupActive{g: g, active: g.GetActiveMember()})
	}

	sub.SetNodes(proxies)

	// Rebuild each group referencing this subscription. The active member is
	// preserved when it still exists in the new pool.
	for _, ga := range groups {
		g := ga.g
		g.RebuildProxies()
		if ga.active != "" {
			found := false
			for _, m := range g.Members {
				if m.Name == ga.active {
					found = true
					break
				}
			}
			if !found {
				g.ActiveMember = ""
			}
		}
		util.Logger.Printf("group %s refreshed from sub %s: %d nodes, %d members", g.Name, sub.Name, len(sub.SubProxies), len(g.Members))
	}
	return nil
}
