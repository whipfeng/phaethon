package tun

import (
	"context"
	"errors"
	"fmt"
	"net"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"phaethon/config"
	"phaethon/connlog"
	"phaethon/dialer"
	"phaethon/mesh"
	"phaethon/util"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// TUNMapping is a special mapping that represents traffic entering through
// the TUN interface. Rules can use "#TUN" suffix to target TUN traffic.
var TUNMapping = &config.Mapping{Name: "TUN", Type: "tun"}

// Engine manages the TUN device, netstack, and traffic interception.
type Engine struct {
	ruleConf   *config.RuleConfiguration
	device     Device
	netstack   *mesh.Netstack
	fakeIP    *mesh.FakeIPPool
	dnsHijack *mesh.DNSHijacker
	routeMgr  *RouteManager
	dhcpSrv   DHCPServer
	prefixLen int
	dataDir   string

	mu sync.Mutex

	// TUN device state (optional, independent of stack)
	tunRunning bool           // TUN device is active
	tunCloseCh chan struct{}  // TUN goroutine exit signal
	tunWG      sync.WaitGroup // TUN goroutines (readLoop/writeLoop)

	// packet counters for diagnostics
	readPackets  atomic.Uint64
	writePackets atomic.Uint64

	// stats notification with debounce
	statsNotifyMu    sync.Mutex
	statsNotifyTimer *time.Timer

	logMu sync.Mutex
	logs  []string

	// meshInterceptor diverts mesh-subnet packets before netstack.
	// Returns true if the packet was handled.
	meshInterceptor func(dstIP net.IP, data []byte) bool
	localMeshVIPs   map[string]bool // all local mesh VIPs as string keys
	meshSubnet      *net.IPNet      // mesh subnet for Fake-IP allocation (set when mesh is enabled)
	meshNetwork     *net.IPNet      // overall mesh network (e.g., 100.0.0.0/8) for identifying mesh IPs
	natTable        *mesh.NATTable    // shared NAT table for TUN and mesh NAT
	modeBTable      *mesh.ModeBTable  // Mode B (proxy entry) connection tracking
	localMeshNodeID string          // local mesh node ID for nodeID.phn → 127.0.0.1 resolution

	meshOutboundCh chan meshOutboundPacket // queue for async mesh interception
	meshWriteCh    chan []byte             // queue for async WriteMeshPacket to TUN device

	// preConnectCallback is called after bind (port allocated) but before connect (SYN sent).
	// Used for ModeBTable registration before the forwarder is triggered.
	preConnectCallback func(dstAddr tcpip.Address, dstPort, srcPort uint16)

	adminHandler AdminHandler // direct admin server connection handler (bypasses OS network stack)
}

// AdminHandler can serve a single network connection directly.
// Implemented by admin.AdminServer to allow bypassing the OS network stack.
type AdminHandler interface {
	ServeConn(conn net.Conn)
}

type meshOutboundPacket struct {
	dstIP net.IP
	data  []byte
}

// NewEngine creates a new TUN engine. It does not start anything yet.
func NewEngine(ruleConf *config.RuleConfiguration) *Engine {
	return &Engine{
		ruleConf: ruleConf,
	}
}

// SetDataDir sets the runtime data directory for persistent storage (e.g. DHCP leases).
func (e *Engine) SetDataDir(dir string) {
	e.dataDir = dir
}

// SetNetstack sets the mesh.Netstack instance for this engine.
// Must be called before Start().
func (e *Engine) SetNetstack(ns *mesh.Netstack) {
	e.netstack = ns
}

// GetNetstack returns the mesh.Netstack instance.
func (e *Engine) GetNetstack() *mesh.Netstack {
	return e.netstack
}

// SetMeshInterceptor registers a callback to intercept packets destined for the mesh subnet.
// The callback returns true if it handled the packet (mesh will forward it).
func (e *Engine) SetMeshInterceptor(handler func(dstIP net.IP, data []byte) bool, localVIPs []net.IP) {
	e.meshInterceptor = handler
	e.localMeshVIPs = make(map[string]bool, len(localVIPs))
	for _, vip := range localVIPs {
		if v4 := vip.To4(); v4 != nil {
			e.localMeshVIPs[v4.String()] = true
		}
	}
	e.meshOutboundCh = make(chan meshOutboundPacket, 65536)
	e.tunWG.Add(1)
	go e.meshOutboundLoop()

	// Share callbacks with the netstack for writeLoop routing
	if e.netstack != nil {
		e.netstack.SetMeshInterceptor(handler)
		e.netstack.SetLocalMeshVIPFunc(e.isLocalMeshVIP)
		e.netstack.SetIsMeshIPFunc(e.isMeshIP)
	}

	util.LogDebug("tun: mesh interceptor set (localVIPs=%v)", localVIPs)
}

// SetNATTable sets the shared NAT table for TUN source NAT and reverse NAT.
func (e *Engine) SetNATTable(nat *mesh.NATTable) {
	e.natTable = nat
	if e.netstack != nil {
		e.netstack.SetNATTable(nat)
	}
}

// SetModeBTable sets the Mode B connection tracking table.
func (e *Engine) SetModeBTable(t *mesh.ModeBTable) {
	e.modeBTable = t
}

// SetPreConnectCallback sets a callback that is called after bind (port allocated)
// but before connect (SYN sent). Used for ModeBTable registration.
func (e *Engine) SetPreConnectCallback(callback func(dstAddr tcpip.Address, dstPort, srcPort uint16)) {
	e.preConnectCallback = callback
}

// GetModeBTable returns the Mode B connection tracking table.
func (e *Engine) GetModeBTable() *mesh.ModeBTable {
	return e.modeBTable
}

// SetLocalMeshNodeID sets the local mesh node ID for nodeID.phn → 127.0.0.1 resolution.
func (e *Engine) SetLocalMeshNodeID(nodeID string) {
	e.localMeshNodeID = nodeID
	util.LogDebug("tun: local mesh nodeID set: %s", nodeID)
}

// Write writes data to the TUN device. Implements the WriteLoopDevice interface
// for mesh.Netstack.
func (e *Engine) Write(data []byte) (int, error) {
	e.mu.Lock()
	dev := e.device
	e.mu.Unlock()
	if dev == nil {
		return 0, fmt.Errorf("TUN device not available")
	}
	return dev.Write(data)
}

// SetAdminHandler sets the admin server for direct connection handling.
// When set, connections to nodeid.phn will be served directly without going through OS network stack.
func (e *Engine) SetAdminHandler(h AdminHandler) {
	e.adminHandler = h
	util.LogDebug("tun: admin handler set for direct connection handling")
}

// SetDNSDomainResolver registers a callback on the DNS hijacker that returns
// the remote Fake-IP subnet for a domain. Returns (subnet, needsFail):
//   - subnet != nil: matched a route, forward to remote
//   - subnet == nil && needsFail == true: static match but node not ready, return SERVFAIL
//   - subnet == nil && needsFail == false: no match, fallback to local pool
func (e *Engine) SetDNSDomainResolver(resolver func(domain string) (*net.IPNet, bool)) {
	util.LogInfo("[DNS-DEBUG] Engine.SetDNSDomainResolver() called")
	if e.dnsHijack != nil {
		e.dnsHijack.SetDomainResolver(resolver)
		util.LogInfo("[DNS-DEBUG] DNS hijacker resolver set successfully")
	} else {
		util.LogWarn("[DNS-DEBUG] DNS hijacker is nil, cannot set resolver")
	}
}

// SetDNSHijacker binds the mesh's DNS hijacker to this engine's netstack.
// Called when mesh is configured and TUN engine is started.
func (e *Engine) SetDNSHijacker(h *mesh.DNSHijacker, fakeIP *mesh.FakeIPPool) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.netstack == nil || !e.netstack.IsRunning() {
		return fmt.Errorf("tun: netstack not running")
	}

	// Bind netstack to the hijacker
	h.BindNetstack(e.netstack.Stack(), e.netstack.Addr(), e.netstack.DNSAddr())

	// Start the hijacker
	if err := h.Start(e.netstack.WaitGroup()); err != nil {
		return fmt.Errorf("tun: start dns hijacker: %w", err)
	}

	e.dnsHijack = h
	e.fakeIP = fakeIP
	if fakeIP != nil {
		fakeIP.SetOnChange(e.notifyStatsChanged)
	}

	util.LogInfo("tun: DNS hijacker bound from mesh")
	return nil
}

// GetFakeIPPool returns the Fake-IP pool for external use (e.g., mesh DNS allocator).
func (e *Engine) GetFakeIPPool() *mesh.FakeIPPool {
	return e.fakeIP
}

// GetDNSHijacker returns the DNS hijacker for external use (e.g., mesh DNS forwarding).
func (e *Engine) GetDNSHijacker() *mesh.DNSHijacker {
	return e.dnsHijack
}

// ResolveDomain resolves a domain name by delegating to the netstack.
func (e *Engine) ResolveDomain(domain string) (net.IP, error) {
	if e.netstack == nil {
		return nil, fmt.Errorf("netstack not initialized")
	}
	return e.netstack.ResolveDomain(domain)
}

// NetDial dials a connection through the netstack.
func (e *Engine) NetDial(network, addr string) (net.Conn, error) {
	if e.netstack == nil {
		return nil, fmt.Errorf("netstack not initialized")
	}
	return e.netstack.NetDial(network, addr)
}

// NetDialWithModeB dials through the netstack and registers in ModeBTable before sending SYN.
func (e *Engine) NetDialWithModeB(network, addr string, clientAddr string, inbound string, mapping *config.Mapping) (net.Conn, error) {
	if e.netstack == nil {
		return nil, fmt.Errorf("netstack not initialized")
	}
	return e.netstack.NetDialWithModeB(network, addr, clientAddr, inbound, mapping, e.modeBTable)
}

// ConfigureMeshAddresses reconfigures the TUN engine to use mesh subnet addresses.
func (e *Engine) ConfigureMeshAddresses(subnet *net.IPNet) error {
	if e.netstack == nil {
		return fmt.Errorf("netstack not initialized")
	}
	if err := e.netstack.ConfigureMeshAddresses(subnet); err != nil {
		return err
	}
	// Also store meshSubnet locally for readLoop/writeLoop routing
	if subnet != nil {
		e.meshSubnet = subnet
		e.netstack.SetMeshSubnet(subnet)

		ip4 := subnet.IP.To4()
		ones, _ := subnet.Mask.Size()
		if ones > 28 {
			e.prefixLen = 24
		} else {
			e.prefixLen = 29
		}
		hostIP := net.IP{ip4[0], ip4[1], ip4[2], ip4[3] + 2}
		util.LogDebug("tun: mesh addresses: hostIP=%s prefixLen=%d", hostIP, e.prefixLen)
	}
	return nil
}

func (e *Engine) isLocalMeshVIP(ip net.IP) bool {
	if e.localMeshVIPs == nil {
		return false
	}
	return e.localMeshVIPs[ip.To4().String()]
}

// InjectMeshPacket injects a raw IP packet into the netstack as if received from the TUN device.
// Delegates to the mesh.Netstack.
func (e *Engine) InjectMeshPacket(data []byte) error {
	if e.netstack == nil {
		return fmt.Errorf("netstack not initialized")
	}
	return e.netstack.InjectMeshPacket(data)
}

// meshWriteLoop consumes packets from meshWriteCh and writes them to the TUN device.
// Decouples mesh writes from callers to prevent blocking.
func (e *Engine) meshWriteLoop() {
	defer e.tunWG.Done()
	for {
		select {
		case <-e.tunCloseCh:
			return
		case data := <-e.meshWriteCh:
			e.mu.Lock()
			dev := e.device
			tunUp := e.tunRunning
			e.mu.Unlock()
			if !tunUp || dev == nil {
				continue
			}
			if _, err := dev.Write(data); err != nil {
				util.LogWarn("tun: meshWriteLoop device.Write failed: %v", err)
			}
		}
	}
}

// WriteMeshPacket writes a raw IP packet directly to the TUN device so the OS
// kernel receives it as an incoming packet from the adapter.
func (e *Engine) WriteMeshPacket(data []byte) error {
	e.mu.Lock()
	tunUp := e.tunRunning
	e.mu.Unlock()

	if !tunUp {
		return fmt.Errorf("TUN not ready (running=%v)", tunUp)
	}
	if len(data) >= 20 {
		srcIP := net.IP(data[12:16])
		dstIP := net.IP(data[16:20])
		util.LogDebug("tun: WriteMeshPacket %s -> %s len=%d", srcIP, dstIP, len(data))
	}

	// Queue for async write to TUN device
	dataCopy := make([]byte, len(data))
	copy(dataCopy, data)
	select {
	case e.meshWriteCh <- dataCopy:
		return nil
	default:
		return fmt.Errorf("meshWriteCh full, dropping packet")
	}
}

// AddMeshRoute adds a route for the mesh subnet through the TUN device.
// This is called when mesh is configured to ensure mesh-destined packets reach the TUN.
func (e *Engine) AddMeshRoute(subnet string) error {
	return e.addMeshRoute(subnet)
}

// SetMeshNetwork sets the overall mesh network range for identifying mesh IPs.
func (e *Engine) SetMeshNetwork(network *net.IPNet) {
	e.meshNetwork = network
}

// isMeshIP checks if an IP belongs to the mesh network (full range, not just local subnet).
func (e *Engine) isMeshIP(ip net.IP) bool {
	if e.meshNetwork != nil && e.meshNetwork.Contains(ip) {
		return true
	}
	if e.meshSubnet != nil && e.meshSubnet.Contains(ip) {
		return true
	}
	return false
}

// AddMeshVIP registers a mesh virtual IP with the gVisor netstack.
// Delegates to the mesh.Netstack.
func (e *Engine) AddMeshVIP(vip net.IP) error {
	if e.netstack == nil {
		return fmt.Errorf("netstack not initialized")
	}
	return e.netstack.AddMeshVIP(vip)
}

// resolveForDirect resolves a domain name to IP addresses for DIRECT connections.
// It uses configured direct-nameserver if available, otherwise falls back to
// ResolveRouteAware which uses the original DNS servers captured at TUN startup.
func (e *Engine) resolveForDirect(domain string) ([]net.IP, error) {
	servers := e.ruleConf.TUN.DirectNameserverList()
	if len(servers) > 0 {
		return resolveWithServers(domain, servers)
	}
	// Fallback: use captured original DNS servers
	ipStrs, err := dialer.ResolveRouteAware(domain)
	if err != nil {
		return nil, err
	}
	var ips []net.IP
	for _, ipStr := range ipStrs {
		if ip := net.ParseIP(ipStr); ip != nil {
			ips = append(ips, ip)
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no IP addresses resolved for %s", domain)
	}
	return ips, nil
}

// resolveWithServers resolves a domain using the specified DNS servers.
// It queries all servers concurrently and returns the first successful result.
// Sockets are bound to the interface determined by routing (excluding TUN interface).
func resolveWithServers(domain string, servers []string) ([]net.IP, error) {
	type result struct {
		ips []net.IP
		err error
	}
	ch := make(chan result, len(servers))

	bc := dialer.GetGlobalBindContext()

	// Shared context to cancel outstanding queries when first success arrives
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, server := range servers {
		go func(s string) {
			serverIP := net.ParseIP(s)
			r := &net.Resolver{
				PreferGo: true,
				Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
					d := net.Dialer{Timeout: 3 * time.Second}
					if bc != nil {
						d.Control = func(network, address string, c syscall.RawConn) error {
							return bc.BindSocket(c, serverIP)
						}
					}
					return d.DialContext(ctx, "udp", net.JoinHostPort(s, "53"))
				},
			}
			queryCtx, queryCancel := context.WithTimeout(ctx, 30*time.Second)
			defer queryCancel()
			ips, err := r.LookupIP(queryCtx, "ip4", domain)
			ch <- result{ips, err}
		}(server)
	}

	// Wait for first successful result or all failures
	var lastErr error
	for range servers {
		res := <-ch
		if res.err == nil && len(res.ips) > 0 {
			cancel() // Cancel remaining queries
			return res.ips, nil
		}
		lastErr = res.err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("all DNS servers failed for %s", domain)
}

// IsEnabled reports whether the TUN engine is active.
func (e *Engine) IsEnabled() bool {
	return e.netstack != nil && e.netstack.IsRunning()
}

// IsTUNRunning reports whether the TUN device is active.
func (e *Engine) IsTUNRunning() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.tunRunning
}

const maxTUNLogs = 32

func (e *Engine) logEvent(format string, args ...interface{}) {
	e.logMu.Lock()
	defer e.logMu.Unlock()
	msg := fmt.Sprintf(format, args...)
	e.logs = append(e.logs, msg)
	if len(e.logs) > maxTUNLogs {
		e.logs = e.logs[len(e.logs)-maxTUNLogs:]
	}
}

// Logs returns the most recent TUN engine event log entries.
func (e *Engine) Logs() []string {
	e.logMu.Lock()
	defer e.logMu.Unlock()
	out := make([]string, len(e.logs))
	copy(out, e.logs)
	return out
}

// notifyStatsChanged schedules a "tun" version bump with debounce.
// Called when packet counters or fakeIP stats change.
func (e *Engine) notifyStatsChanged() {
	e.statsNotifyMu.Lock()
	defer e.statsNotifyMu.Unlock()

	if e.statsNotifyTimer != nil {
		return // already scheduled
	}

	// Use 2-second debounce to avoid excessive API calls from rapidly changing counters
	e.statsNotifyTimer = time.AfterFunc(2*time.Second, func() {
		e.statsNotifyMu.Lock()
		e.statsNotifyTimer = nil
		e.statsNotifyMu.Unlock()

		util.DefaultVersionNotifier.BumpVersion("tun")
	})
}

// RouteSnapshot returns the current route manager state.
func (e *Engine) RouteSnapshot() RouteSnapshot {
	e.mu.Lock()
	rm := e.routeMgr
	e.mu.Unlock()
	if rm == nil {
		return RouteSnapshot{
			Exclusions:   []string{},
			SplitTunnels: []string{},
		}
	}
	return rm.Snapshot()
}

// TUNInterfaceIndex returns the OS interface index of the phaethon TUN adapter.
// The watchdog uses this to bind its HTTP probe sockets directly to the TUN
// interface so probe traffic cannot bypass TUN.
func (e *Engine) TUNInterfaceIndex() int {
	e.mu.Lock()
	rm := e.routeMgr
	e.mu.Unlock()
	if rm == nil {
		return 0
	}
	return rm.TUNInterfaceIndex()
}

// PhysicalInterfaceIndex returns the OS interface index of the original default
// interface (before TUN was activated). The watchdog uses this to bind
// DNS queries to the original default interface, bypassing TUN split-tunnel routes.
func (e *Engine) PhysicalInterfaceIndex() int {
	e.mu.Lock()
	rm := e.routeMgr
	e.mu.Unlock()
	if rm == nil {
		return 0
	}
	return rm.DefaultIfaceIndex
}

// TUNInterfaceIP returns the IPv4 address assigned to the phaethon TUN adapter.
// The watchdog uses this as the source address for its HTTP probes so Windows
// routes the packets into the Wintun ring.
func (e *Engine) TUNInterfaceIP() net.IP {
	e.mu.Lock()
	rm := e.routeMgr
	e.mu.Unlock()
	if rm == nil {
		return nil
	}
	return rm.TUNInterfaceIP()
}

// TUNStats contains diagnostic statistics from the TUN engine.
type TUNStats struct {
	ReadPackets  uint64          `json:"readPackets"`
	WritePackets uint64          `json:"writePackets"`
	FakeIP       mesh.FakeIPStats `json:"fakeIP"`
}

// Stats returns a snapshot of the TUN engine diagnostic statistics.
func (e *Engine) Stats() TUNStats {
	s := TUNStats{
		ReadPackets:  e.readPackets.Load(),
		WritePackets: e.writePackets.Load(),
	}
	e.mu.Lock()
	fakeIP := e.fakeIP
	e.mu.Unlock()
	if fakeIP != nil {
		s.FakeIP = fakeIP.Stats()
	}
	return s
}

// DHCPLeaseSnapshot returns the current DHCP lease list, or nil if DHCP is not running.
func (e *Engine) DHCPLeaseSnapshot() []DHCPLease {
	e.mu.Lock()
	srv := e.dhcpSrv
	e.mu.Unlock()
	if srv == nil {
		return nil
	}
	return srv.Leases()
}

// UpdateDHCPStaticBindings hot-updates the DHCP static bindings if the server is running.
func (e *Engine) UpdateDHCPStaticBindings(bindings []config.DHCPStaticBinding) {
	e.mu.Lock()
	srv := e.dhcpSrv
	e.mu.Unlock()
	if srv != nil {
		srv.UpdateStaticBindings(bindings)
	}
}

// StartStack starts the gVisor netstack (FakeIP, DNS hijacker, TCP/UDP forwarders).
// This is the core networking layer and should always be running.
func (e *Engine) StartStack() error {
	if e.netstack == nil {
		return fmt.Errorf("tun: netstack not set (call SetNetstack before Start)")
	}

	// Set up forwarder callbacks on the netstack
	e.netstack.SetCallbacks(&mesh.ForwarderCallbacks{
		HandleTCPConn: func(conn net.Conn, srcAddr, dstAddr string, dstPort int, inbound string, modeBMapping *config.Mapping) {
			e.handleConn(conn, srcAddr, dstAddr, dstPort, inbound, modeBMapping)
		},
		HandleUDPConn: func(conn net.Conn, srcAddr, dstAddr string, dstPort int, inbound string, modeBMapping *config.Mapping) {
			e.handleUDP(conn, srcAddr, dstAddr, dstPort, inbound, modeBMapping)
		},
		ResolveOriginalSrc: func(proto int, srcIP net.IP, srcPort uint16) (net.IP, bool) {
			if e.natTable != nil {
				origIP, _ := e.natTable.ResolveOriginalSrc(byte(proto), srcIP, srcPort)
				return origIP, true
			}
			return srcIP, false
		},
		LookupModeB: func(proto int, dstIP net.IP, dstPort, srcPort uint16) (string, string, *config.Mapping) {
			if e.modeBTable != nil {
				return e.modeBTable.LookupByDst(byte(proto), dstIP, dstPort, srcPort)
			}
			return "", "", nil
		},
		IsMeshIP: func(ip net.IP) bool {
			return e.isMeshIP(ip)
		},
		StatsNotify: func() {
			e.writePackets.Add(1)
			e.notifyStatsChanged()
		},
	})

	// Set writeLoop device and close channel
	e.netstack.WriteLoopDevice = e
	e.netstack.WriteLoopCloseCh = func() <-chan struct{} {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.tunCloseCh != nil {
			return e.tunCloseCh
		}
		// Return netstack closeCh if TUN is not running
		return e.netstack.CloseCh()
	}

	// Set mesh outbound callbacks for writeLoop
	e.netstack.MeshOutboundFunc = func(dstIP net.IP, data []byte) bool {
		if e.meshOutboundCh == nil {
			return false
		}
		select {
		case e.meshOutboundCh <- meshOutboundPacket{dstIP: dstIP, data: data}:
			return true
		default:
			return false
		}
	}
	e.netstack.MeshOutboundFullFunc = func(dstIP net.IP, data []byte) {
		// Re-inject for local delivery as fallback
		if e.netstack != nil {
			e.netstack.InjectInbound(ipv4.ProtocolNumber, data)
		}
	}

	if err := e.netstack.Start(); err != nil {
		return fmt.Errorf("tun: start netstack: %w", err)
	}

	e.logEvent("gVisor netstack started")
	util.LogDebug("gVisor netstack started")
	return nil
}

// StartTUN starts the TUN device, configures OS routes, and redirects system DNS.
// Requires StartStack() to be called first.
func (e *Engine) StartTUN() error {
	e.mu.Lock()
	if e.netstack == nil || !e.netstack.IsRunning() {
		e.mu.Unlock()
		return fmt.Errorf("stack not running, call StartStack() first")
	}
	if e.tunRunning {
		e.mu.Unlock()
		return fmt.Errorf("TUN already running")
	}

	// Ensure admin privileges (Windows UAC auto-elevation)
	if err := EnsureAdminPrivileges(); err != nil {
		e.mu.Unlock()
		e.logEvent("TUN ensure admin privileges failed: %v", err)
		connlog.Log("TUN", "SYSTEM", "", "", "", 0, nil, "fail", fmt.Errorf("admin privileges: %w", err))
		return err
	}

	// Clean up residual resources from previous abnormal exit
	CleanupResidual()

	// Create TUN device
	dev, err := CreateDevice()
	if err != nil {
		e.mu.Unlock()
		e.logEvent("TUN create device failed: %v", err)
		connlog.Log("TUN", "SYSTEM", "", "", "", 0, nil, "fail", fmt.Errorf("create device: %w", err))
		return fmt.Errorf("tun: create device: %w", err)
	}
	e.device = dev

	// LAN/private subnets should bypass TUN to avoid breaking local network
	// connectivity. Proxy server exclusion routes are intentionally omitted:
	// outbound sockets are bound to the correct physical interface by the
	// dialer package, so proxy traffic does not loop back into TUN.
	e.routeMgr = NewRouteManager(dev.Name(), dev.GUID())
	if e.ruleConf != nil && e.ruleConf.TUN != nil {
		e.routeMgr.bypassGateway = e.ruleConf.TUN.IsBypassGateway()
	}
	// On Windows the Wintun adapter LUID is available immediately; passing it in
	// avoids waiting for the TCP/IP stack to register the adapter by name.
	if luidGetter, ok := dev.(interface{ LUID() uint64 }); ok {
		e.routeMgr.SetTUNLUID(luidGetter.LUID())
	}
	e.routeMgr.SetExclusions(DefaultLANExclusions)

	addr := e.netstack.Addr()
	hostIP := net.IP(addr.AsSlice())
	if err := e.routeMgr.Setup(hostIP.String(), e.prefixLen); err != nil {
		e.logEvent("TUN setup routes failed: %v", err)
		connlog.Log("TUN", "SYSTEM", "", "", "", 0, nil, "fail", fmt.Errorf("setup routes: %w", err))
		dev.Close()
		e.device = nil
		e.mu.Unlock()
		return fmt.Errorf("tun: setup routes: %w", err)
	}

	// Inject the captured network context into the dialer package so all
	// outbound connections bind to the correct physical interface.
	dialer.SetGlobalBindContext(&dialer.BindContext{
		DefaultIfaceName:   e.routeMgr.DefaultIfaceName,
		DefaultIfaceIndex:  e.routeMgr.DefaultIfaceIndex,
		TUNLUID:            e.routeMgr.TUNLUID(),
		TUNIfaceName:       dev.Name(),
		OriginalDNSServers: e.routeMgr.OriginalDNSServers,
	})

	// Start TUN-level goroutines
	e.tunRunning = true
	e.tunCloseCh = make(chan struct{})
	e.meshWriteCh = make(chan []byte, 32768)
	e.mu.Unlock()

	e.tunWG.Add(1)
	go e.meshWriteLoop()

	e.tunWG.Add(1)
	go e.readLoop()

	// Redirect system DNS to the dedicated DNS address in the TUN subnet
	// so applications send queries that route through TUN to DNSHijacker.
	dnsAddr := e.netstack.DNSAddr()
	dnsIP := net.IP(dnsAddr.AsSlice())
	if err := setSystemDNS(dev.Name(), dnsIP.String()); err != nil {
		util.LogWarn("tun: failed to set system dns: %v", err)
	}

	if e.ruleConf != nil {
		go dialer.PreWarmSSHProxies(e.ruleConf.Proxies)
	}

	// Start DHCP server if bypass-gateway and DHCP are both enabled.
	if e.ruleConf != nil && e.ruleConf.TUN != nil &&
		e.ruleConf.TUN.IsBypassGateway() && e.ruleConf.TUN.IsDHCPEnabled() {
		ifaceName := ""
		if e.ruleConf.TUN.DHCP != nil && e.ruleConf.TUN.DHCP.Interface != "" {
			ifaceName = e.ruleConf.TUN.DHCP.Interface
		} else if e.routeMgr != nil {
			ifaceName = e.routeMgr.DefaultIfaceName
		}
		dnsAddr2 := e.netstack.DNSAddr()
		dnsIPIP := net.IP(dnsAddr2.AsSlice())
		srv, err := newDHCPServer(ifaceName, e.ruleConf.TUN.DHCP, dnsIPIP, e.dataDir)
		if err != nil {
			util.LogWarn("dhcp: failed to create server: %v", err)
		} else if srv != nil {
			if err := srv.Start(); err != nil {
				util.LogWarn("dhcp: failed to start: %v", err)
			} else {
				e.dhcpSrv = srv
				util.LogDebug("dhcp: server started on %s", ifaceName)
			}
		}
	}

	e.logEvent("TUN device started on %s", dev.Name())
	connlog.Log("TUN", "SYSTEM", "", "", dev.Name(), 0, nil, "ok", nil)
	util.LogDebug("TUN device started on %s", dev.Name())
	return nil
}

// Start starts the gVisor netstack and optionally the TUN device.
// Convenience method equivalent to StartStack() + StartTUN() (if TUN is enabled).
func (e *Engine) Start() error {
	if err := e.StartStack(); err != nil {
		return err
	}
	tunEnabled := e.ruleConf != nil && e.ruleConf.TUN != nil && e.ruleConf.TUN.IsEnabled()
	if tunEnabled {
		if err := e.StartTUN(); err != nil {
			e.StopStack()
			return err
		}
	}
	return nil
}

// StopTUN stops the TUN device, restores routes and system DNS.
// The gVisor netstack continues running.
func (e *Engine) StopTUN() error {
	e.mu.Lock()
	if !e.tunRunning {
		e.mu.Unlock()
		return nil
	}
	e.tunRunning = false
	close(e.tunCloseCh)
	e.mu.Unlock()

	// Clear the global bind context so subsequent dials resume normal behavior.
	dialer.SetGlobalBindContext(nil)

	// Restore system DNS first while the TUN adapter still exists.
	if e.device != nil {
		restoreSystemDNS(e.device.Name())
	}

	// Stop DHCP server before tearing down routes.
	if e.dhcpSrv != nil {
		e.dhcpSrv.Stop()
		e.dhcpSrv = nil
	}

	// Teardown routes while the adapter still has a valid LUID/index.
	if e.routeMgr != nil {
		e.routeMgr.Teardown()
		e.routeMgr = nil
	}

	// Close device to unblock readLoop (stuck on ReceivePacket)
	// This also ends the Wintun session and deletes the adapter.
	if e.device != nil {
		e.device.Close()
		e.device = nil
	}

	// Wait for TUN goroutines to finish
	e.tunWG.Wait()

	e.logEvent("TUN device stopped")
	util.LogDebug("TUN device stopped (netstack still running)")
	return nil
}

// StopStack stops the gVisor netstack (DNS hijacker, forwarders, etc.).
// Should be called after StopTUN() if TUN was running.
func (e *Engine) StopStack() error {
	if e.netstack == nil {
		return nil
	}

	// Stop services before waiting for goroutines, since service goroutines
	// (e.g. DNS hijacker) are part of wg and need their endpoints closed to exit.
	if e.dnsHijack != nil {
		e.dnsHijack.Stop()
	}

	if err := e.netstack.Stop(); err != nil {
		return err
	}

	e.logEvent("gVisor netstack stopped")
	util.LogDebug("gVisor netstack stopped")
	return nil
}

// Stop tears down everything: TUN device and gVisor netstack.
// Convenience method equivalent to StopTUN() + StopStack().
func (e *Engine) Stop() error {
	e.StopTUN()
	e.StopStack()
	connlog.Log("TUN", "SYSTEM", "", "", "", 0, nil, "stopped", nil)
	return nil
}

// AddHTunnelEndpoint creates a link.Endpoint for an h_tunnel proxy.
// Delegates to the mesh.Netstack.
func (e *Engine) AddHTunnelEndpoint(proxyName string, peer mesh.PeerSender) error {
	if e.netstack == nil {
		return fmt.Errorf("netstack not initialized")
	}
	return e.netstack.AddHTunnelEndpoint(proxyName, peer)
}

// RemoveHTunnelEndpoint removes and closes the h_tunnel endpoint for a proxy.
func (e *Engine) RemoveHTunnelEndpoint(proxyName string) {
	if e.netstack != nil {
		e.netstack.RemoveHTunnelEndpoint(proxyName)
	}
}

// HTunnelEndpoint returns the h_tunnel endpoint for a proxy, or nil if not found.
func (e *Engine) HTunnelEndpoint(proxyName string) *mesh.HTunnelEndpoint {
	if e.netstack == nil {
		return nil
	}
	return e.netstack.HTunnelEndpoint(proxyName)
}

// meshOutboundLoop consumes packets from meshOutboundCh and calls meshInterceptor.
// If the interceptor returns false (packet not handled by mesh), re-inject into netstack.
func (e *Engine) meshOutboundLoop() {
	defer e.tunWG.Done()
	for {
		select {
		case <-e.tunCloseCh:
			return
		case pkt := <-e.meshOutboundCh:
			if e.meshInterceptor != nil && !e.meshInterceptor(pkt.dstIP, pkt.data) {
				// Diagnostic: log non-mesh packets being re-injected to netstack (advertised route path)
				if len(pkt.data) >= 20 && pkt.data[0]>>4 == 4 {
					isMesh := e.meshSubnet != nil && e.meshSubnet.Contains(pkt.dstIP)
					if !isMesh {
						util.LogDebug("[TCP-DIAG] reinject to netstack: src=%s dst=%s proto=%d len=%d",
							net.IP(pkt.data[12:16]), pkt.dstIP, pkt.data[9], len(pkt.data))
					}
				}
				if e.netstack != nil {
					e.netstack.InjectInbound(ipv4.ProtocolNumber, pkt.data)
				}
			}
		}
	}
}

// readLoop reads IP packets from the TUN device and injects them into netstack.
func (e *Engine) readLoop() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer e.tunWG.Done()
	readBuf := make([]byte, 2048)
	for {
		select {
		case <-e.tunCloseCh:
			return
		default:
		}

		// Read into a temporary buffer first, then copy to a per-packet buffer
		// so netstack owns the data and cannot be overwritten by the next read.
		n, err := e.device.Read(readBuf)
		if err != nil {
			select {
			case <-e.tunCloseCh:
				return
			default:
				if errors.Is(err, ErrSessionClosed) {
					util.LogError("tun: session closed, stopping read loop: %v", err)
					return
				}
				if errors.Is(err, syscall.EAGAIN) {
					continue
				}
				util.LogWarn("tun: read error: %v", err)
				continue
			}
		}
		if n == 0 {
			continue
		}
		e.readPackets.Add(1)
		e.notifyStatsChanged()

		// Determine network protocol from the IP version field.
		var proto tcpip.NetworkProtocolNumber
		switch readBuf[0] >> 4 {
		case 4:
			proto = ipv4.ProtocolNumber
		case 6:
			proto = ipv6.ProtocolNumber
		default:
			util.LogWarn("tun: dropped non-IP packet (version=%d)", readBuf[0]>>4)
			continue
		}

		// Log inbound packets for debugging. Cap total noise by only logging the
		// first 200 packets at info level; Fake-IP packets are always logged.
		if proto == ipv4.ProtocolNumber && n >= 20 {
			dstIP := net.IP(readBuf[16:20])
			srcIP := net.IP(readBuf[12:16]).String()
			ipProto := readBuf[9]
			if e.meshSubnet != nil && e.meshSubnet.Contains(dstIP) {
				util.LogDebug("tun read FAKE: %s -> %s (proto=%d len=%d cnt=%d)", srcIP, dstIP, ipProto, n, e.readPackets.Load())
			} else if e.readPackets.Load() <= 200 {
				util.LogDebug("tun read: %s -> %s (proto=%d len=%d)", srcIP, dstIP.String(), ipProto, n)
			}
		}

		pktBuf := make([]byte, n)
		copy(pktBuf, readBuf[:n])

		// NAT: replace src IP with VIP for all IPv4 packets.
		// Must run BEFORE mesh interception so mesh always sees VIP source IPs.
		if e.natTable != nil && n >= 20 && pktBuf[0]>>4 == 4 {
			if natPkt := e.natTable.TranslateOutbound(pktBuf); natPkt != nil {
				pktBuf = natPkt
			}
		}

		// Debug: log ALL TCP packets to mesh network (100.0.0.0/8)
		if n >= 40 && pktBuf[0]>>4 == 4 && pktBuf[9] == 6 { // TCP
			dstIP := net.IP(pktBuf[16:20])
			if dstIP[0] == 100 { // 100.0.0.0/8
				dstPort := uint16(pktBuf[22])<<8 | uint16(pktBuf[23])
				util.LogDebug("[TCP-DEBUG] readLoop entry: TCP dst=%s:%d src=%s", dstIP, dstPort, net.IP(pktBuf[12:16]))
			}
		}

		// Mesh interception: let mesh layer decide if packet should be routed via mesh.
		// The mesh interceptor checks its routing table (including gateway routes) to determine
		// if the packet should be sent via mesh or handled normally.
		// At this point, src is already a mesh IP (VIP), so mesh layer won't need to NAT.
		if proto == ipv4.ProtocolNumber && n >= 20 {
			dstIP := net.IP(pktBuf[16:20])
			if e.meshInterceptor != nil {
				// Debug: log TCP packets to mesh subnet
				if e.meshSubnet != nil && e.meshSubnet.Contains(dstIP) && pktBuf[9] == 6 { // TCP
					srcPort := uint16(pktBuf[20])<<8 | uint16(pktBuf[21])
					dstPort := uint16(pktBuf[22])<<8 | uint16(pktBuf[23])
					util.LogDebug("[TCP-DEBUG] readLoop: TCP to mesh subnet dst=%s:%d src=%s:%d",
						dstIP, dstPort, net.IP(pktBuf[12:16]), srcPort)
				}
				select {
				case e.meshOutboundCh <- meshOutboundPacket{dstIP: dstIP, data: pktBuf}:
					continue
				default:
					util.LogWarn("[MESH-DIAG] meshOutboundCh full in readLoop (%d pending), falling through to netstack", len(e.meshOutboundCh))
				}
			}
		}

		if e.netstack != nil && e.netstack.LinkEP() != nil {
			pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(pktBuf)})
			e.netstack.LinkEP().InjectInbound(proto, pkt)
			pkt.DecRef()
		}
	}
}

// relay bidirectionally copies data between conn and target.
//
// Idle timeout: 5 minutes (aggressive, like NAT/firewall tables). TCP protocol
// has no mechanism to negotiate or communicate idle timeouts between peers, so
// this is a unilateral decision by the proxy. Applications that need long-lived
// connections must configure their own keepalive at the application layer:
//   - SSH: ServerAliveInterval / ClientAliveInterval
//   - Database: connection pool keepalive / validation queries
//   - HTTP: Keep-Alive headers
//
// Design principle: the proxy is "dumb" and aggressively reclaims resources.
// Connection lifetime management is the responsibility of the endpoints.
func relay(conn, target net.Conn) {
	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())

	done := make(chan struct{})
	defer close(done)

	// Watchdog: close connection if idle for 5 minutes. This reclaims resources
	// from abandoned connections (e.g., NAT drops mapping, client crashes without
	// sending FIN). Applications must send data within 5 minutes to keep alive.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if time.Since(time.Unix(0, lastActivity.Load())) > 5*time.Minute {
					util.LogInfo("[TUN] relay idle timeout: connection idle for 5 minutes, closing")
					conn.Close()
					target.Close()
					return
				}
			}
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			lastActivity.Store(time.Now().UnixNano())
			n, err := conn.Read(buf)
			if n > 0 {
				lastActivity.Store(time.Now().UnixNano())
				if _, werr := target.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				if cw, ok := target.(interface{ CloseWrite() error }); ok {
					cw.CloseWrite()
				}
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			lastActivity.Store(time.Now().UnixNano())
			n, err := target.Read(buf)
			if n > 0 {
				lastActivity.Store(time.Now().UnixNano())
				if _, werr := conn.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				if cw, ok := conn.(interface{ CloseWrite() error }); ok {
					cw.CloseWrite()
				}
				return
			}
		}
	}()

	wg.Wait()
}

// handleUDP relays UDP datagrams between netstack and the real network via proxy or direct.
// It preserves datagram boundaries by reading/writing one datagram at a time.
func (e *Engine) handleUDP(netstackConn net.Conn, srcAddr string, dstAddr string, dstPort int, inbound string, modeBMapping *config.Mapping) {
	if inbound == "" {
		inbound = "TUN"
	}

	// Check if this is a Fake-IP: restore original domain.
	var domain string
	if d := e.fakeIP.LookupDomain(dstAddr); d != "" {
		domain = d
		util.LogDebug("tun: udp fake-ip %s -> %s", dstAddr, domain)
	}

	// Check if this is a local mesh nodeID domain (nodeID.phn → 127.0.0.1)
	var localNodeDomain bool
	if domain != "" && e.localMeshNodeID != "" {
		expectedDomain := e.localMeshNodeID + ".phn"
		if domain == expectedDomain {
			localNodeDomain = true
			util.LogDebug("tun: local mesh nodeID domain detected (udp): %s -> 127.0.0.1", domain)
		}
	}

	connID := util.NextConnID()

	// Use domain for rule matching (so domain-based rules work).
	matchAddr := dstAddr
	if domain != "" {
		matchAddr = domain
	}

	req := config.NewConnectRequest("udp", matchAddr, dstPort)
	var proxy *config.Proxy
	var matchResult *config.MatchResult
	if e.ruleConf != nil {
		matchMapping := TUNMapping
		if modeBMapping != nil {
			matchMapping = modeBMapping
		}
		req, proxy, matchResult = e.ruleConf.ResolveMatch(req, matchMapping)
	}

	resolvedAddr := req.DstAddr
	resolvedPort := req.DstPort
	rec := connlog.Start(inbound, "UDP", srcAddr, matchAddr).Resolve(resolvedAddr, resolvedPort, matchResult)
	defer rec.Close()

	if proxy != nil && strings.ToUpper(proxy.Type) == config.ProxyREJECT {
		util.LogDebug("[%s] [%s] udp %s:%d -> REJECTED", inbound, connID, resolvedAddr, resolvedPort)
		rec.Reject()
		return
	}

	var targetConn net.PacketConn
	var err error
	var dialIP net.IP

	if proxy != nil && strings.ToUpper(proxy.Type) != config.ProxyDIRECT {
		targetConn, err = dialer.ChainUDPDial(proxy)
		if err != nil {
			util.LogWarn("[%s] [%s] udp dial %s:%d via %s fail: %v", inbound, connID, resolvedAddr, resolvedPort, proxy.Name, err)
			rec.Fail(err)
			return
		}
		dialIP = net.ParseIP(resolvedAddr)
	} else {
		// Direct dial: resolvedAddr may be the original domain or a resolver-rewritten
		// host (IP or domain). If it is still a domain, resolve it; if it is an IP,
		// dial it directly.
		dialIP = net.ParseIP(resolvedAddr)
		if localNodeDomain {
			// Local mesh nodeID domain: connect to localhost
			dialIP = net.ParseIP("127.0.0.1")
			util.LogDebug("[%s] [%s] udp local mesh nodeID domain: %s -> %s", inbound, connID, domain, dialIP)
		} else if dialIP == nil {
			// Resolve the real IP for DIRECT connections.
			ips, err := e.resolveForDirect(resolvedAddr)
			if err != nil || len(ips) == 0 {
				util.LogWarn("[%s] [%s] udp resolve %s fail: %v", inbound, connID, resolvedAddr, err)
				rec.Fail(err)
				return
			}
			// Prefer IPv4
			for _, ip := range ips {
				if ip4 := ip.To4(); ip4 != nil {
					dialIP = ip4
					break
				}
			}
			util.LogDebug("[%s] [%s] udp resolved %s -> %s for DIRECT", inbound, connID, resolvedAddr, dialIP)
		}
		targetConn, err = dialer.ListenPacketBoundTo("udp", "", dialIP)
		if err != nil {
			util.LogWarn("[%s] [%s] udp direct dial %s:%d fail: %v", inbound, connID, resolvedAddr, resolvedPort, err)
			rec.SetDst(dialIP.String()).Fail(err)
			return
		}
	}
	defer targetConn.Close()

	// Use the resolved IP for the destination address.
	dstUDPAddr := &net.UDPAddr{IP: dialIP, Port: resolvedPort}
	util.LogDebug("[%s] [%s] udp %s:%d -> %s", inbound, connID, resolvedAddr, resolvedPort, proxyDesc(proxy))
	rec.Establish(connID, proxy, proxy.IsDirect())

	relayUDP(netstackConn, targetConn, dstUDPAddr)
}

// relayUDP copies datagrams between the netstack UDP connection and the target
// PacketConn, preserving datagram boundaries. Uses a watchdog goroutine to
// enforce a 30-second idle timeout, preventing goroutine leaks when the remote
// stops responding.
func relayUDP(netstackConn net.Conn, targetConn net.PacketConn, dstAddr *net.UDPAddr) {
	const bufSize = 65535
	const idleTimeout = 30 * time.Second

	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())

	done := make(chan struct{})
	defer close(done)

	// Watchdog: force-close when idle
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if time.Since(time.Unix(0, lastActivity.Load())) > idleTimeout {
					netstackConn.Close()
					targetConn.Close()
					return
				}
			}
		}
	}()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, bufSize)
		for {
			n, err := netstackConn.Read(buf)
			if err != nil {
				return
			}
			lastActivity.Store(time.Now().UnixNano())
			if _, err := targetConn.WriteTo(buf[:n], dstAddr); err != nil {
				return
			}
		}
	}()

	buf := make([]byte, bufSize)
	for {
		n, _, err := targetConn.ReadFrom(buf)
		if err != nil {
			break
		}
		lastActivity.Store(time.Now().UnixNano())
		if _, err := netstackConn.Write(buf[:n]); err != nil {
			break
		}
	}
	wg.Wait()
}

// handleConn routes a TUN-side TCP connection through the proxy chain or direct.
func (e *Engine) handleConn(conn net.Conn, srcAddr string, dstAddr string, dstPort int, inbound string, modeBMapping *config.Mapping) {
	if inbound == "" {
		inbound = "TUN"
	}
	util.LogDebug("[TCP-DEBUG] handleConn called dst=%s:%d", dstAddr, dstPort)
	defer conn.Close()

	// Check if this is a Fake-IP: restore original domain.
	var domain string
	if e.fakeIP != nil {
		if d := e.fakeIP.LookupDomain(dstAddr); d != "" {
			domain = d
			util.LogDebug("[TCP-DEBUG] fake-ip lookup: %s -> %s", dstAddr, domain)
		} else {
			util.LogDebug("[TCP-DEBUG] fake-ip lookup: %s -> (no domain)", dstAddr)
		}
	}

	// Check if this is a local mesh nodeID domain (nodeID.phn → 127.0.0.1)
	var localNodeDomain bool
	if domain != "" && e.localMeshNodeID != "" {
		expectedDomain := e.localMeshNodeID + ".phn"
		if domain == expectedDomain {
			localNodeDomain = true
			util.LogDebug("[TCP-DEBUG] local mesh nodeID domain: %s == %s -> will dial 127.0.0.1", domain, expectedDomain)
		} else {
			util.LogDebug("[TCP-DEBUG] domain %s != expected %s", domain, expectedDomain)
		}
	} else {
		util.LogDebug("[TCP-DEBUG] localNodeDomain check skipped: domain=%q localMeshNodeID=%q", domain, e.localMeshNodeID)
	}

	connID := util.NextConnID()

	// A dst inside the fake-IP alloc range with no mapping means the client is
	// using a stale fake-IP (pool was reset, e.g. node restart). Close right
	// away instead of direct-dialing a dead address for ~20s; the client's
	// retry re-resolves DNS and gets the fresh fake-IP.
	if domain == "" && e.fakeIP != nil {
		if ip := net.ParseIP(dstAddr); ip != nil && e.fakeIP.InAllocRange(ip) {
			util.LogWarn("[TUN] [%s] stale fake-IP dst=%s:%d src=%s rejected (no pool mapping, node restarted?)",
				connID, dstAddr, dstPort, srcAddr)
			return
		}
	}

	// Diagnostic: log Mode B connections prominently
	if modeBMapping != nil {
		util.LogInfo("[TCP-DIAG] [%s] Mode B conn: src=%s dst=%s:%d mapping=%s inbound=%s",
			connID, srcAddr, dstAddr, dstPort, modeBMapping.Name, inbound)
	}

	// Use domain for rule matching (so domain-based rules work).
	matchAddr := dstAddr
	if domain != "" {
		matchAddr = domain
	}

	req := config.NewConnectRequest("tcp", matchAddr, dstPort)
	var proxy *config.Proxy
	var matchResult *config.MatchResult
	if e.ruleConf != nil {
		matchMapping := TUNMapping
		if modeBMapping != nil {
			matchMapping = modeBMapping
		}
		req, proxy, matchResult = e.ruleConf.ResolveMatch(req, matchMapping)
		if proxy != nil {
			util.LogDebug("[TCP-DEBUG] rule match: %s:%d -> proxy=%s type=%s", matchAddr, dstPort, proxy.Name, proxy.Type)
		} else {
			util.LogDebug("[TCP-DEBUG] rule match: %s:%d -> DIRECT (no proxy matched)", matchAddr, dstPort)
		}
	}

	// Use the (possibly redirected) destination from Resolving
	resolvedAddr := req.DstAddr
	resolvedPort := req.DstPort
	rec := connlog.Start(inbound, "TCP", srcAddr, matchAddr).Resolve(resolvedAddr, resolvedPort, matchResult)
	defer rec.Close()

	var targetConn net.Conn
	var err error

	if proxy != nil && strings.ToUpper(proxy.Type) == config.ProxyREJECT {
		util.LogDebug("[%s] [%s] %s:%d -> REJECTED", inbound, connID, resolvedAddr, resolvedPort)
		rec.Reject()
		return
	}

	if localNodeDomain {
		// Direct admin handler: bypass OS network stack entirely
		if e.adminHandler != nil {
			util.LogDebug("[TCP-DEBUG] [%s] localNodeDomain=true, serving via admin handler directly (bypassing OS network stack)", connID)
			rec.SetDst("localhost").Establish(connID, proxy, false)
			e.adminHandler.ServeConn(conn)
			return
		}
		// Fallback: dial localhost via OS network stack
		localIP := dialer.GetLocalIPForDial(nil)
		dialAddr := localIP.String()
		util.LogDebug("[TCP-DEBUG] [%s] localNodeDomain=true, dialing local %s:%d (bypassing proxy)", connID, dialAddr, resolvedPort)
		targetConn, err = dialer.DialRouteAware("tcp", net.JoinHostPort(dialAddr, strconv.Itoa(resolvedPort)))
		if err != nil {
			util.LogWarn("[%s] [%s] local dial %s:%d fail: %v", inbound, connID, dialAddr, resolvedPort, err)
			rec.SetDst(dialAddr).Fail(err)
			return
		}
	} else if proxy != nil && strings.ToUpper(proxy.Type) != config.ProxyDIRECT {
		util.LogDebug("[TCP-DEBUG] [%s] dialing via proxy %s: %s:%d", connID, proxy.Name, resolvedAddr, resolvedPort)
		targetConn, err = dialer.ChainDialWithID(proxy, resolvedAddr, resolvedPort, connID)
		if err != nil {
			util.LogWarn("[%s] [%s] dial %s:%d via %s fail: %v", inbound, connID, resolvedAddr, resolvedPort, proxy.Name, err)
			rec.Fail(err)
			return
		}
		util.LogDebug("[TCP-DEBUG] [%s] proxy dial success", connID)
	} else {
		// Direct dial: resolvedAddr may be the original domain or a resolver-rewritten
		// host (IP or domain). If it is still a domain, resolve it; if it is an IP,
		// dial it directly.
		dialAddr := resolvedAddr
		if net.ParseIP(dialAddr) == nil {
			// Resolve the real IP for DIRECT connections.
			util.LogDebug("[TCP-DEBUG] [%s] resolving %s for DIRECT dial", connID, dialAddr)
			ips, err := e.resolveForDirect(dialAddr)
			if err != nil || len(ips) == 0 {
				util.LogWarn("[%s] [%s] resolve %s fail: %v", inbound, connID, dialAddr, err)
				rec.Fail(err)
				return
			}
			// Prefer IPv4
			for _, ip := range ips {
				if ip4 := ip.To4(); ip4 != nil {
					dialAddr = ip4.String()
					break
				}
			}
			util.LogDebug("[TCP-DEBUG] [%s] resolved %s -> %s", connID, resolvedAddr, dialAddr)
		}
		util.LogDebug("[TCP-DEBUG] [%s] dialing tcp %s:%d", connID, dialAddr, resolvedPort)
		targetConn, err = dialer.DialRouteAware("tcp", net.JoinHostPort(dialAddr, fmt.Sprintf("%d", resolvedPort)))
		if err != nil {
			util.LogWarn("[%s] [%s] direct dial %s:%d fail: %v", inbound, connID, dialAddr, resolvedPort, err)
			rec.SetDst(dialAddr).Fail(err)
			return
		}
		util.LogDebug("[TCP-DEBUG] [%s] direct dial success, starting relay", connID)
	}
	defer targetConn.Close()

	util.LogDebug("[TCP-DEBUG] [%s] relay started: %s:%d -> %s", connID, resolvedAddr, resolvedPort, proxyDesc(proxy))
	rec.Establish(connID, proxy, proxy.IsDirect())
	relay(conn, targetConn)
}

func proxyDesc(p *config.Proxy) string {
	if p == nil || strings.EqualFold(p.Type, config.ProxyDIRECT) {
		return "DIRECT"
	}
	return p.Name
}
