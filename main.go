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
	"phaethon/dialer"
	"phaethon/reverse"
	"phaethon/server"
	"phaethon/tun"
	"phaethon/util"
)

//go:embed conf/default.yaml
var defaultConfig []byte

// Version is set via -ldflags at build time.
var Version = "dev"

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
		if r.tunRes != nil {
			r.tunRes.Stop()
		}
		if r.adminServer != nil {
			r.adminServer.Close()
		}
	})
}

// toggleTUN starts or stops the TUN engine on the current active resources.
func toggleTUN(enable bool, resPtr **activeResources) error {
	res := *resPtr
	if res == nil || res.ruleConf == nil {
		return fmt.Errorf("runtime not ready")
	}
	if enable {
		if res.tunRes != nil && res.tunRes.engine != nil && res.tunRes.engine.IsEnabled() {
			return nil
		}
		res.tunRes = startTUNIfEnabled(res.ruleConf)
		if res.tunRes == nil || res.tunRes.engine == nil || !res.tunRes.engine.IsEnabled() {
			return fmt.Errorf("TUN start failed")
		}
		return nil
	}
	if res.tunRes != nil {
		res.tunRes.Stop()
		res.tunRes = nil
	}
	return nil
}

// buildTUNStatus returns the current TUN state for the admin API.
func buildTUNStatus(res *activeResources) map[string]interface{} {
	if res == nil || res.ruleConf == nil {
		return map[string]interface{}{
			"available":  tun.Available(),
			"enabled":    true,
			"running":    false,
			"deviceName": "",
			"routes": map[string]interface{}{
				"applied":           false,
				"tunIP":             "",
				"defaultIface":      "",
				"defaultIfaceIndex": 0,
				"originalGateway":   "",
				"exclusions":        []string{},
				"splitTunnels":      []string{},
			},
			"logs":      []string{},
			"stats":     tun.TUNStats{},
			"probeURLs": []string{},
		}
	}
	enabled := res.ruleConf.TUN == nil || res.ruleConf.TUN.Enabled == nil || *res.ruleConf.TUN.Enabled
	status := map[string]interface{}{
		"available":  tun.Available(),
		"enabled":    enabled,
		"running":    false,
		"deviceName": "",
		"routes": map[string]interface{}{
			"applied":           false,
			"tunIP":             "",
			"defaultIface":      "",
			"defaultIfaceIndex": 0,
			"originalGateway":   "",
			"exclusions":        []string{},
			"splitTunnels":      []string{},
		},
		"logs":      []string{},
		"probeURLs": []string{},
		"stats":     tun.TUNStats{},
	}
	if res.tunRes != nil && res.tunRes.engine != nil {
		engine := res.tunRes.engine
		status["running"] = engine.IsEnabled()
		status["routes"] = engine.RouteSnapshot()
		status["logs"] = engine.Logs()
		status["stats"] = engine.Stats()
		if engine.IsEnabled() {
			status["deviceName"] = "PhaethonTUN"
		}
	} else {
		status["stats"] = tun.TUNStats{}
	}
	probeURLs := tun.DefaultProbeURLs
	if res.ruleConf != nil && res.ruleConf.TUN != nil && len(res.ruleConf.TUN.ProbeURLList()) > 0 {
		probeURLs = res.ruleConf.TUN.ProbeURLList()
	}
	status["probeURLs"] = probeURLs
	return status
}

var runCnt int

// subCacheDir is the subscription cache directory, set during getRuleConf.
// It is kept outside the conf/ directory so subscription cache writes never
// mix with user configuration files.
var subCacheDir string

// dataDir is the unified runtime data directory (.phaethon/).
// All non-configuration persistent data (reverse-id, bindings, subscription cache)
// is stored here, separate from the static conf/ directory.
var dataDir = filepath.Join(".", ".phaethon")

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

	res := &activeResources{
		ruleConf:           ruleConf,
		mappingListeners:   make(map[string]net.Listener),
		mappingReverse:     make(map[string]*server.ReverseServer),
		reverseClientStops: make(map[string]chan struct{}),
	}

	// Start TUN engine if enabled (intercepts system-level traffic)
	res.tunRes = startTUNIfEnabled(ruleConf)

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

	// 2. Load config.yaml from working directory.
	configFile := filepath.Join(workDir, "config.yaml")
	var ruleConf *config.RuleConfiguration
	if _, err := os.Stat(configFile); err == nil {
		util.Logger.Printf("configPath=%s", configFile)
		conf, err := config.LoadRaw(configFile)
		if err != nil {
			util.Logger.Printf("ERROR: load config fail: %v", err)
		} else {
			ruleConf = conf
		}
	} else if os.IsNotExist(err) {
		// Write the embedded default config to disk on first run, substituting
		// a generated admin token so users have a working config immediately.
		util.Logger.Printf("no config.yaml found, generating initial config at %s", configFile)
		if genErr := writeDefaultConfig(configFile); genErr != nil {
			util.Logger.Printf("ERROR: generate initial config fail: %v", genErr)
		} else {
			conf, loadErr := config.LoadRaw(configFile)
			if loadErr != nil {
				util.Logger.Printf("ERROR: load generated config fail: %v", loadErr)
			} else {
				ruleConf = conf
			}
		}
	}

	// 3. Fall back to embedded default config.
	if ruleConf == nil && len(defaultConfig) > 0 {
		util.Logger.Printf("no config.yaml found, using built-in default config")
		conf, err := config.LoadRawBytes(defaultConfig)
		if err != nil {
			util.Logger.Printf("ERROR: load default config fail: %v", err)
		} else {
			ruleConf = conf
		}
	}

	if ruleConf == nil {
		util.Logger.Printf("No valid configuration found!")
		return nil
	}

	// Pre-init so that Match() works for subscription routing
	if err := ruleConf.Init(); err != nil {
		util.Logger.Printf("ERROR: pre-init config fail: %v", err)
		return nil
	}

	// 4. Ensure every instance has a stable ReverseID (even pure registry instances
	//    with no reverse configs need one for identification in the admin UI).
	//    ReverseID is stored in .phaethon/setup/reverse-id file.
	instanceID := loadOrGenerateInstanceReverseID(ruleConf)
	if instanceID != "" && ruleConf != nil {
		ruleConf.ReverseID = instanceID
	}

	// 5. Normalize reverse configs: assign Seq numbers to any config lacking one.
	normalizeReverseConfigs(ruleConf)

	// 7. Load subscription node pools from the on-disk cache so groups have nodes
	// immediately without blocking startup on network I/O. Missing or stale caches
	// are refreshed in the background once the listeners are up.
	subCacheDir = filepath.Join(dataDir, "subscription")
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
// Priority: ruleConf.ReverseID > .phaethon/setup/reverse-id file > generate new.
// If a new one is generated, it is saved to the reverse-id file.
func loadOrGenerateInstanceReverseID(ruleConf *config.RuleConfiguration) string {
	if ruleConf != nil && ruleConf.ReverseID != "" {
		return ruleConf.ReverseID
	}
	// Load from .phaethon/setup/reverse-id file
	dataDir := filepath.Join(".phaethon", "setup")
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
	// Watchdog mode must be handled before any normal initialization so the
	// child process only monitors the parent and does not start services.
	if wdPid := os.Getenv("LAYER_WATCHDOG_PID"); wdPid != "" {
		runWatchdog(wdPid)
		os.Exit(0)
	}

	// Extract Java-style -Dkey=value arguments up front so they do not confuse
	// the standard flag parser, while remaining available via util.JavaProp().
	os.Args = util.SetJavaProps(os.Args)

	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println(Version)
		return
	}

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

	// Load config first
	ruleConf := getRuleConf()

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
			return toggleTUN(enable, &resources)
		}
		resources.adminServer.GetTUNStatus = func() map[string]interface{} {
			return buildTUNStatus(resources)
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
			if resources != nil {
				resources.closeAll()
			}
			return
		case <-consoleCloseNotify():
			util.Logger.Printf("Received console close event, shutting down...")
			if resources != nil {
				resources.closeAll()
			}
			return
		}
	}
}

// writeStartupError persists a fatal startup error to disk so the user can
// read it even if the console window closes immediately (e.g. double-click
// on Windows).
func writeStartupError(err error) error {
	path := filepath.Join(dataDir, "startup-error.log")
	_ = os.MkdirAll(dataDir, 0755)
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
			util.Logger.Printf("[REVERSE-CLIENT] stopping reverse client name=%s addr=%s", rc.Name, rc.OutboundProxy)
			return
		case <-globalStop:
			util.Logger.Printf("[REVERSE-CLIENT] stopping reverse client name=%s addr=%s", rc.Name, rc.OutboundProxy)
			return
		default:
		}

		err := runReverseSession(rc, ruleConf, configStop, globalStop)
		if err != nil {
			util.Logger.Printf("[REVERSE-CLIENT] session error (name=%s addr=%s): %v, reconnecting in %v...", rc.Name, rc.OutboundProxy, err, interval)
		} else {
			util.Logger.Printf("[REVERSE-CLIENT] session ended (name=%s addr=%s): %v, reconnecting in %v...", rc.Name, rc.OutboundProxy, err, interval)
		}

		select {
		case <-configStop:
			util.Logger.Printf("[REVERSE-CLIENT] stopping reverse client name=%s addr=%s", rc.Name, rc.OutboundProxy)
			return
		case <-globalStop:
			util.Logger.Printf("[REVERSE-CLIENT] stopping reverse client name=%s addr=%s", rc.Name, rc.OutboundProxy)
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

	if rc.OutboundProxy == "" {
		return fmt.Errorf("reverse client requires outbound-proxy (must connect to registry through proxy chain)")
	}
	outboundProxy := ruleConf.ProxyNames[rc.OutboundProxy]
	if outboundProxy == nil {
		return fmt.Errorf("outbound proxy not found: %s", rc.OutboundProxy)
	}

	cc := dialer.NewControlClient(outboundProxy)
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
	clientID := ruleConf.ReverseID

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
		Proto:            outboundProxy.Type,
		PreferredPort:    rc.PreferredPort,
		ListenerProto:    rc.ListenerProto,
		ListenerUser:     rc.ListenerUser,
		ListenerPassword: rc.ListenerPassword,
		ListenerSNI:      rc.ListenerSNI,
		DirectDstHost:    rc.DirectDstHost,
		DirectDstPort:    rc.DirectDstPort,
		OutboundProxy:    rc.OutboundProxy,
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
	host := outboundProxy.Server
	if host == "" {
		host = rc.OutboundProxy
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
		ReverseProxy:          rc.OutboundProxy,
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
	admin := resources.adminServer
	admin.RefreshSubscription = func(subName string) error {
		conf := admin.GetConfig()
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
	admin.CheckGroupHealth = func(groupName string) error {
		conf := admin.GetConfig()
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
	admin.CheckGroupTest = func(groupName string) error {
		conf := admin.GetConfig()
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
	admin.CheckProxyHealth = func(groupName, proxyName string) (config.HealthInfo, error) {
		conf := admin.GetConfig()
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
			if !m.FromSubscription {
				// Manual proxy: run a real TCP connectivity check.
				info := checkManualProxyHealth(p)
				g.SetHealthImmediate(key, info.Alive, info.Latency)
				saveHealthState(conf)
				return g.GetHealthInfo(key), nil
			}
			testURL := g.HealthCheckURL
			if testURL == "" {
				testURL = "http://www.google.com/generate_204"
			}
			host, port, useHTTP := parseTestURL(testURL)
			path := getTestPath(testURL)
			alive, latency := checkProxyHealth(p, host, port, useHTTP, path)
			g.SetHealthImmediate(key, alive, latency)
			saveHealthState(conf)
			return g.GetHealthInfo(key), nil
		}
		return config.HealthInfo{}, fmt.Errorf("group not found: %s", groupName)
	}
	admin.CheckSubscriptionHealth = func(subName, nodeName, url string) (config.HealthInfo, error) {
		conf := admin.GetConfig()
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
	admin.CheckManualProxyHealth = func(proxyName string) (config.HealthInfo, error) {
		conf := admin.GetConfig()
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
		start := time.Now()
		// Dial through the proxy chain (Next hop) to reach this proxy's server
		var conn net.Conn
		var err error
		if p.Next != nil && !strings.EqualFold(p.Next.Type, config.ProxyDIRECT) {
			// Use the proxy chain to connect to this proxy's server
			nextDialer := dialer.NewDialer(p.Next)
			conn, err = nextDialer.Dial(p.Server, p.Port)
		} else {
			// No next hop or next hop is DIRECT: connect directly
			addr := net.JoinHostPort(p.Server, strconv.Itoa(p.Port))
			conn, err = net.DialTimeout("tcp", addr, 5*time.Second)
		}
		if err != nil {
			return config.HealthInfo{Alive: false, LastCheck: time.Now()}, nil
		}
		conn.Close()
		return config.HealthInfo{Alive: true, Latency: time.Since(start), LastCheck: time.Now()}, nil
	}
	admin.GetReverseBindings = func() []server.PortBinding {
		if server.GlobalControlManager == nil {
			return nil
		}
		return server.GlobalControlManager.GetBindings()
	}
	admin.ForceRemoveBinding = func(reverseID string, seq int) error {
		if server.GlobalControlManager == nil {
			return fmt.Errorf("control manager not available")
		}
		return server.GlobalControlManager.ForceRemoveBinding(reverseID, seq)
	}
	admin.OnIncrementalUpdate = func() error {
		// mergeAndInitLocked already updated s.conf in place, which is the same
		// object as resources.ruleConf. All servers see the changes immediately.
		activeRuleConf.Store(resources.ruleConf)
		return nil
	}
	admin.OnMappingUpdate = func(old, newMapping *config.Mapping) error {
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
	admin.OnReverseConfigUpdate = func(old, newConfig *config.ReverseConfig) error {
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
	if testURL == "" {
		testURL = "http://www.google.com/generate_204"
	}

	host, port, useHTTP := parseTestURL(testURL)
	path := getTestPath(testURL)

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
			if ok {
				selected := innerGroup.NextWithVisited(make(map[string]bool))
				g.SetHealth(key, selected != nil, 0)
			} else {
				g.SetHealth(key, false, 0)
			}
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
		// Manual proxies are assumed alive; only subscription nodes are checked.
		if !m.FromSubscription {
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
			alive, latency := checkProxyHealth(it.p, host, port, useHTTP, path)
			g.SetHealth(it.key, alive, latency)
		}(it)
	}
	wg.Wait()

	saveHealthState(ruleConf)
	util.DefaultVersionNotifier.BumpVersion("stats")
}

// checkManualProxyHealth performs a simple TCP connectivity check for a manual
// proxy. DIRECT and REJECT are always reported as alive without dialing.
func checkManualProxyHealth(p *config.Proxy) config.HealthInfo {
	if strings.EqualFold(p.Type, config.ProxyDIRECT) || strings.EqualFold(p.Type, config.ProxyREJECT) {
		return config.HealthInfo{Alive: true, LastCheck: time.Now()}
	}
	start := time.Now()
	addr := net.JoinHostPort(p.Server, strconv.Itoa(p.Port))
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return config.HealthInfo{Alive: false, LastCheck: time.Now()}
	}
	conn.Close()
	return config.HealthInfo{Alive: true, Latency: time.Since(start), LastCheck: time.Now()}
}

// checkGroupTest runs an immediate one-off test for every member of a group.
// Unlike the periodic checkGroupHealth, this tests manual proxies too and uses
// SetHealthImmediate so the result is visible right away.
func checkGroupTest(ruleConf *config.RuleConfiguration, g *config.ProxyGroup) {
	testURL := g.HealthCheckURL
	if testURL == "" {
		testURL = "http://www.google.com/generate_204"
	}
	host, port, useHTTP := parseTestURL(testURL)
	path := getTestPath(testURL)

	members := g.GetMembers()
	var wg sync.WaitGroup
	sem := make(chan struct{}, healthCheckConcurrency)
	for _, m := range members {
		key := m.HealthKey()
		if m.IsGroup {
			ruleConf.RLock()
			innerGroup, ok := ruleConf.GroupNames[m.Name]
			ruleConf.RUnlock()
			alive := false
			if ok {
				alive = innerGroup.NextWithVisited(make(map[string]bool)) != nil
			}
			g.SetHealthImmediate(key, alive, 0)
			continue
		}

		p := g.ResolveMember(m)
		if p == nil {
			g.SetHealthImmediate(key, false, 0)
			continue
		}
		if strings.EqualFold(p.Type, config.ProxyDIRECT) || strings.EqualFold(p.Type, config.ProxyREJECT) {
			g.SetHealthImmediate(key, true, 0)
			continue
		}
		if !m.FromSubscription {
			wg.Add(1)
			sem <- struct{}{}
			go func(key string, p *config.Proxy) {
				defer wg.Done()
				defer func() { <-sem }()
				info := checkManualProxyHealth(p)
				g.SetHealthImmediate(key, info.Alive, info.Latency)
			}(key, p)
			continue
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(key string, p *config.Proxy) {
			defer wg.Done()
			defer func() { <-sem }()
			alive, latency := checkProxyHealth(p, host, port, useHTTP, path)
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
	return filepath.Join(dataDir, "health-state.json")
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
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		util.LogWarn("[HEALTH] create data dir fail: %v", err)
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
	req := config.NewConnectRequest(subHost, subPort)
	proxy := ruleConf.Match(req, nil)

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
