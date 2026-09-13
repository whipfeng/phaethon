package tun

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"phaethon/config"
	"phaethon/connlog"
	"phaethon/dialer"
	"phaethon/util"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/loopback"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// TUNMapping is a special mapping that represents traffic entering through
// the TUN interface. Rules can use "#TUN" suffix to target TUN traffic.
var TUNMapping = &config.Mapping{Name: "TUN", Type: "tun"}

// Engine manages the TUN device, netstack, and traffic interception.
type Engine struct {
	ruleConf  *config.RuleConfiguration
	device    Device
	linkEP    *channel.Endpoint
	ns        *stack.Stack
	fakeIP    *FakeIPPool
	dnsHijack *DNSHijacker
	routeMgr  *RouteManager
	dhcpSrv   DHCPServer
	addr      tcpip.Address
	dnsAddr   tcpip.Address
	prefixLen int
	dataDir   string

	mu      sync.Mutex
	running bool      // gVisor stack is running
	closeCh chan struct{} // gVisor stack close signal
	wg      sync.WaitGroup // gVisor stack goroutines

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
	meshSubnet      *net.IPNet      // mesh subnet for Fake-IP allocation (nil = use default 198.18.0.0/15)
}

// NewEngine creates a new TUN engine. It does not start anything yet.
func NewEngine(ruleConf *config.RuleConfiguration) *Engine {
	return &Engine{
		ruleConf: ruleConf,
		closeCh:  make(chan struct{}),
	}
}

// SetDataDir sets the runtime data directory for persistent storage (e.g. DHCP leases).
func (e *Engine) SetDataDir(dir string) {
	e.dataDir = dir
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
	util.LogInfo("tun: mesh interceptor set (localVIPs=%v)", localVIPs)
}

// SetMeshDNSResolver registers a callback to resolve mesh domain names (e.g., node.phn) to VIPs.
func (e *Engine) SetMeshDNSResolver(resolver func(domain string) net.IP) {
	if e.dnsHijack != nil {
		e.dnsHijack.MeshResolver = resolver
		util.LogInfo("tun: mesh DNS resolver set")
	}
}

// SetMeshDNSNetstackForwarder registers a callback to forward DNS queries via netstack socket to gateway GIP.
func (e *Engine) SetMeshDNSNetstackForwarder(forwarder func(domain string) (net.IP, error)) {
	if e.dnsHijack != nil {
		e.dnsHijack.MeshDNSNetstackForwarder = forwarder
		util.LogInfo("tun: mesh DNS netstack forwarder set")
	}
}

// GetFakeIPPool returns the Fake-IP pool for external use (e.g., mesh DNS allocator).
func (e *Engine) GetFakeIPPool() *FakeIPPool {
	return e.fakeIP
}

// GetDNSHijacker returns the DNS hijacker for external use (e.g., mesh DNS forwarding).
func (e *Engine) GetDNSHijacker() *DNSHijacker {
	return e.dnsHijack
}

// ResolveDomain resolves a domain name by sending a DNS query through the
// netstack UDP socket to the local hijacker (dnsAddr:53). The packet goes
// through the loopback NIC and is handled by the hijacker, which allocates
// a fakeIP from the local pool or forwards to a remote mesh gateway.
func (e *Engine) ResolveDomain(domain string) (net.IP, error) {
	util.LogInfo("netstack: ResolveDomain called for domain=%s", domain)
	e.mu.Lock()
	ns := e.ns
	dnsAddr := e.dnsAddr
	e.mu.Unlock()
	if ns == nil {
		return nil, fmt.Errorf("netstack not running")
	}

	var wq waiter.Queue
	ep, err := ns.NewEndpoint(udp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: new endpoint: %v", domain, err)
	}
	defer ep.Close()

	if err := ep.Bind(tcpip.FullAddress{}); err != nil {
		return nil, fmt.Errorf("resolve %s: bind: %v", domain, err)
	}
	if err := ep.Connect(tcpip.FullAddress{Addr: dnsAddr, Port: 53}); err != nil {
		return nil, fmt.Errorf("resolve %s: connect: %v", domain, err)
	}

	// Register waiter BEFORE write — loopback delivery is synchronous.
	waitEntry, ch := waiter.NewChannelEntry(waiter.EventIn)
	wq.EventRegister(&waitEntry)
	defer wq.EventUnregister(&waitEntry)

	txID := uint16(time.Now().UnixNano())
	query := buildDNSQuery(domain, txID)
	if _, err := ep.Write(&slicePayload{data: query}, tcpip.WriteOptions{}); err != nil {
		return nil, fmt.Errorf("resolve %s: write: %v", domain, err)
	}

	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		return nil, fmt.Errorf("resolve %s: timeout", domain)
	}

	var buf bytes.Buffer
	if _, err := ep.Read(&buf, tcpip.ReadOptions{}); err != nil {
		return nil, fmt.Errorf("resolve %s: read: %v", domain, err)
	}
	fakeIP := parseDNSResponseIP(buf.Bytes())
	if fakeIP == nil {
		return nil, fmt.Errorf("resolve %s: bad response", domain)
	}
	return fakeIP, nil
}

// NetDial dials a connection through the netstack. For addresses in the fakeIP
// or mesh subnet, the connection goes through the loopback NIC and is caught by
// TCP/UDP forwarders, which route to local or remote destinations transparently.
func (e *Engine) NetDial(network, addr string) (net.Conn, error) {
	util.LogInfo("netstack: NetDial called with network=%s addr=%s", network, addr)
	e.mu.Lock()
	running := e.running
	ns := e.ns
	e.mu.Unlock()

	if !running || ns == nil {
		return nil, fmt.Errorf("netstack not running")
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("netstack dial: parse addr: %w", err)
	}
	portNum, err := net.LookupPort(network, port)
	if err != nil {
		return nil, fmt.Errorf("netstack dial: parse port: %w", err)
	}

	ip := net.ParseIP(host)
	if ip == nil {
		return nil, fmt.Errorf("netstack dial: not an IP: %s", host)
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return nil, fmt.Errorf("netstack dial: IPv6 not supported: %s", host)
	}

	var arr [4]byte
	copy(arr[:], ip4)
	remoteAddr := tcpip.FullAddress{
		Addr: tcpip.AddrFrom4(arr),
		Port: uint16(portNum),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	switch network {
	case "tcp", "tcp4":
		util.LogInfo("netstack: DialContextTCP to %s:%d", host, portNum)
		conn, err := gonet.DialContextTCP(ctx, ns, remoteAddr, ipv4.ProtocolNumber)
		if err != nil {
			util.LogWarn("netstack: DialContextTCP failed: %v", err)
			return nil, err
		}
		util.LogInfo("netstack: DialContextTCP succeeded to %s:%d", host, portNum)
		return conn, nil
	case "udp", "udp4":
		return gonet.DialUDP(ns, nil, &remoteAddr, ipv4.ProtocolNumber)
	default:
		return nil, fmt.Errorf("netstack dial: unsupported network: %s", network)
	}
}

// ConfigureMeshAddresses reconfigures the TUN engine to use mesh subnet addresses.
// VIP (.1) = mesh routing + NAT source, hostIP (.2) = TUN adapter, GIP (.3) = netstack/DNS.
// Must be called before Start() or after a full restart.
func (e *Engine) ConfigureMeshAddresses(subnet *net.IPNet) error {
	if subnet == nil {
		return nil
	}
	ip4 := subnet.IP.To4()
	if ip4 == nil {
		return fmt.Errorf("mesh subnet must be IPv4")
	}

	// .1 = VIP (used for NAT source, registered in mesh module)
	// .2 = hostIP (TUN adapter OS side)
	// .3 = GIP (netstack internal, DNS, proxy socket source)
	hostIP := net.IP{ip4[0], ip4[1], ip4[2], ip4[3] + 2}
	gip := net.IP{ip4[0], ip4[1], ip4[2], ip4[3] + 3}

	e.addr = tcpip.AddrFrom4Slice(hostIP)
	e.dnsAddr = tcpip.AddrFrom4Slice(gip)
	e.meshSubnet = subnet

	ones, _ := subnet.Mask.Size()
	if ones > 28 {
		e.prefixLen = 24 // ensure enough room
	} else {
		e.prefixLen = 29
	}

	util.LogInfo("tun: mesh addresses: hostIP=%s GIP=%s", hostIP, gip)
	return nil
}

func (e *Engine) isLocalMeshVIP(ip net.IP) bool {
	if e.localMeshVIPs == nil {
		return false
	}
	return e.localMeshVIPs[ip.To4().String()]
}

// InjectMeshPacket injects a raw IP packet into the netstack as if received from the TUN device.
// Used by the mesh module to deliver received overlay packets to the local TCP/IP stack.
func (e *Engine) InjectMeshPacket(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	var proto tcpip.NetworkProtocolNumber
	switch data[0] >> 4 {
	case 4:
		proto = ipv4.ProtocolNumber
	case 6:
		proto = ipv6.ProtocolNumber
	default:
		return fmt.Errorf("non-IP packet version=%d", data[0]>>4)
	}
	if len(data) >= 20 {
		srcIP := net.IP(data[12:16])
		dstIP := net.IP(data[16:20])
		util.LogDebug("tun: InjectMeshPacket %s -> %s proto=%d len=%d", srcIP, dstIP, proto, len(data))
	}
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(data),
	})
	e.linkEP.InjectInbound(proto, pkt)
	pkt.DecRef()
	return nil
}

// WriteMeshPacket writes a raw IP packet directly to the TUN device so the OS
// kernel receives it as an incoming packet from the adapter.
func (e *Engine) WriteMeshPacket(data []byte) error {
	e.mu.Lock()
	dev := e.device
	running := e.running
	e.mu.Unlock()

	if !running || dev == nil {
		return fmt.Errorf("TUN not ready (running=%v, device=%v)", running, dev != nil)
	}
	if len(data) >= 20 {
		srcIP := net.IP(data[12:16])
		dstIP := net.IP(data[16:20])
		util.LogDebug("tun: WriteMeshPacket %s -> %s len=%d", srcIP, dstIP, len(data))
	}
	_, err := dev.Write(data)
	if err != nil {
		util.LogWarn("tun: WriteMeshPacket device.Write failed: %v", err)
	}
	return err
}

// AddMeshRoute adds a route for the mesh subnet through the TUN device.
// This is called when mesh is enabled to ensure mesh-destined packets reach the TUN.
func (e *Engine) AddMeshRoute(subnet string) error {
	return e.addMeshRoute(subnet)
}

// AddMeshVIP registers a mesh virtual IP with the gVisor netstack so it responds
// to packets (e.g., ICMP) destined for that IP.
func (e *Engine) AddMeshVIP(vip net.IP) error {
	if e.ns == nil {
		return fmt.Errorf("netstack not initialized")
	}
	vip4 := vip.To4()
	if vip4 == nil {
		return fmt.Errorf("only IPv4 mesh VIP supported")
	}
	ap := tcpip.AddressWithPrefix{Address: tcpip.AddrFrom4([4]byte(vip4)), PrefixLen: 32}
	protoAddr := tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: ap,
	}
	if err := e.ns.AddProtocolAddress(outNICID, protoAddr, stack.AddressProperties{}); err != nil {
		return fmt.Errorf("add mesh VIP %s: %v", vip, err)
	}
	util.LogInfo("tun: registered mesh VIP %s with netstack", vip)
	return nil
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
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ips, err := r.LookupIP(ctx, "ip4", domain)
			ch <- result{ips, err}
		}(server)
	}

	// Wait for first successful result or all failures
	var lastErr error
	for range servers {
		res := <-ch
		if res.err == nil && len(res.ips) > 0 {
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
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running
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
	ReadPackets  uint64      `json:"readPackets"`
	WritePackets uint64      `json:"writePackets"`
	FakeIP       FakeIPStats `json:"fakeIP"`
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
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return fmt.Errorf("stack already running")
	}

	// Address determination (always needed for netstack)
	// If ConfigureMeshAddresses was called before Start(), use those addresses.
	// Otherwise fall back to hardcoded defaults for non-mesh mode.
	// hostIP is the address assigned to the TUN adapter (OS side);
	// it must NOT be added as a local netstack address, otherwise replies
	// destined to it from the DNS hijacker / forwarders would be looped back
	// inside netstack instead of being written back to the TUN device.
	// dnsIP is a dedicated DNS address within the TUN subnet. DNSHijacker binds
	// to this address inside netstack. DNS queries are routed through the TUN
	// device to reach it, eliminating the need for a host-side DNS proxy.
	if e.addr == (tcpip.Address{}) {
		hostIP := net.ParseIP("192.0.2.2").To4()
		e.addr = tcpip.AddrFrom4([4]byte(hostIP))
	}
	if e.dnsAddr == (tcpip.Address{}) {
		dnsIP := net.ParseIP("192.0.2.3").To4()
		e.dnsAddr = tcpip.AddrFrom4([4]byte(dnsIP))
	}
	if e.prefixLen == 0 {
		e.prefixLen = 29
	}

	// gVisor netstack
	if err := e.initStack(); err != nil {
		e.mu.Unlock()
		return fmt.Errorf("tun: init netstack: %w", err)
	}

	// Fake-IP pool: use mesh subnet if configured, otherwise default 198.18.0.0/15.
	if e.meshSubnet != nil {
		e.fakeIP = NewFakeIPPoolWithSubnet(e.meshSubnet, 3) // skip .1/.2/.3
	} else {
		e.fakeIP = NewFakeIPPool()
	}
	e.fakeIP.SetOnChange(e.notifyStatsChanged)

	// DNS hijacker
	e.dnsHijack = NewDNSHijacker(e.ns, e.fakeIP, e.addr, e.dnsAddr)
	if err := e.dnsHijack.Start(&e.wg); err != nil {
		e.dnsHijack.Stop()
		e.wg.Wait()
		e.ns.Close()
		e.mu.Unlock()
		return fmt.Errorf("tun: start dns hijacker: %w", err)
	}

	// Start stack-level goroutines
	e.running = true
	e.closeCh = make(chan struct{})
	e.mu.Unlock()

	e.wg.Add(3)
	go e.acceptTCP()
	go e.acceptUDP()
	go e.writeLoop()

	// Diagnostic goroutine: log packet counts every 5 seconds.
	e.wg.Add(1)
	go e.logPacketCounts()

	e.logEvent("gVisor netstack started")
	util.LogInfo("gVisor netstack started")
	return nil
}

// StartTUN starts the TUN device, configures OS routes, and redirects system DNS.
// Requires StartStack() to be called first.
func (e *Engine) StartTUN() error {
	e.mu.Lock()
	if !e.running {
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

	hostIP := net.IP(e.addr.AsSlice())
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
	e.mu.Unlock()

	e.tunWG.Add(1)
	go e.readLoop()

	// Redirect system DNS to the dedicated DNS address in the TUN subnet
	// so applications send queries that route through TUN to DNSHijacker.
	dnsIP := net.IP(e.dnsAddr.AsSlice())
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
		dnsIPIP := net.IP(e.dnsAddr.AsSlice())
		srv, err := newDHCPServer(ifaceName, e.ruleConf.TUN.DHCP, dnsIPIP, e.dataDir)
		if err != nil {
			util.LogWarn("dhcp: failed to create server: %v", err)
		} else if srv != nil {
			if err := srv.Start(); err != nil {
				util.LogWarn("dhcp: failed to start: %v", err)
			} else {
				e.dhcpSrv = srv
				util.LogInfo("dhcp: server started on %s", ifaceName)
			}
		}
	}

	e.logEvent("TUN device started on %s", dev.Name())
	connlog.Log("TUN", "SYSTEM", "", "", dev.Name(), 0, nil, "ok", nil)
	util.LogInfo("TUN device started on %s", dev.Name())
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
	util.LogInfo("TUN device stopped (netstack still running)")
	return nil
}

// StopStack stops the gVisor netstack (DNS hijacker, forwarders, etc.).
// Should be called after StopTUN() if TUN was running.
func (e *Engine) StopStack() error {
	e.mu.Lock()
	if !e.running {
		e.mu.Unlock()
		return nil
	}
	e.running = false
	close(e.closeCh)
	e.mu.Unlock()

	// Wait for stack goroutines to finish
	e.wg.Wait()

	// Stop services
	if e.dnsHijack != nil {
		e.dnsHijack.Stop()
	}
	if e.ns != nil {
		e.ns.Close()
	}

	e.logEvent("gVisor netstack stopped")
	util.LogInfo("gVisor netstack stopped")
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

// initStack creates the gvisor netstack and attaches the link endpoints.
const outNICID = 1
const loNICID = 2

func (e *Engine) initStack() error {
	linkEP := channel.New(512, 1500, "")
	e.linkEP = linkEP

	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	e.ns = s

	// Outbound NIC: handles outbound dispatch (mesh interception, TUN write, VIP return).
	if err := s.CreateNIC(outNICID, linkEP); err != nil {
		return fmt.Errorf("create out nic: %v", err)
	}

	// Loopback NIC: handles outbound packets to local addresses (dnsAddr, fakeIP).
	// WritePackets re-injects packets as inbound, so forwarder/hijacker can catch them.
	loEP := loopback.New()
	if err := s.CreateNIC(loNICID, loEP); err != nil {
		return fmt.Errorf("create loopback nic: %v", err)
	}

	// Register dnsAddr on loopback NIC so the stack recognizes it as local.
	ap := tcpip.AddressWithPrefix{Address: e.dnsAddr, PrefixLen: 32}
	protoAddr := tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: ap,
	}
	if err := s.AddProtocolAddress(loNICID, protoAddr, stack.AddressProperties{}); err != nil {
		return fmt.Errorf("add dns address to loopback: %v", err)
	}

	// Do NOT add 192.0.2.2 as a local netstack address. The TUN adapter IP
	// is configured on the Windows side so the host uses it as the source
	// address for packets entering Wintun. If netstack considered it local,
	// DNS responses (and other replies) destined to 192.0.2.2 would be
	// looped back inside netstack instead of being written back to the Wintun
	// device for the host resolver to receive.

	s.SetPromiscuousMode(outNICID, true)
	s.SetSpoofing(outNICID, true)
	s.SetPromiscuousMode(loNICID, true)
	s.SetSpoofing(loNICID, true)

	// Allow the netstack to forward IPv4/IPv6 packets that are not addressed to
	// a local NIC address. This is required for the Fake-IP scheme: connections
	// to 198.18.0.0/15 are routed to the TUN NIC and must be handled by the
	// TCP/UDP forwarders even though the destination is not assigned locally.
	_ = s.SetForwardingDefaultAndAllNICs(ipv4.ProtocolNumber, true)
	_ = s.SetForwardingDefaultAndAllNICs(ipv6.ProtocolNumber, true)

	// Route table:
	//   1. VIP (.1) → outNIC   — TUN return traffic (NAT src=VIP, return dst=VIP)
	//   2. meshSubnet → outNIC — writeLoop intercepts → meshInterceptor → mesh link
	//   3. default → loNIC     — all other outbound → loopback → forwarder/hijacker
	routes := []tcpip.Route{
		{Destination: header.IPv6EmptySubnet, NIC: loNICID},
	}

	if e.meshSubnet != nil {
		ip4 := e.meshSubnet.IP.To4()
		meshSubnet, _ := tcpip.NewSubnet(tcpip.AddrFrom4Slice(ip4), tcpip.MaskFromBytes(e.meshSubnet.Mask))
		// VIP (.1) — more specific /32 route, must come before meshSubnet
		vipAddr, _ := tcpip.NewSubnet(tcpip.AddrFrom4([4]byte{ip4[0], ip4[1], ip4[2], ip4[3] + 1}), tcpip.MaskFromBytes([]byte{255, 255, 255, 255}))
		routes = append([]tcpip.Route{
			{Destination: vipAddr, NIC: outNICID},
			{Destination: meshSubnet, NIC: outNICID},
		}, routes...)
	}
	// default → loNIC is already in routes

	s.SetRouteTable(routes)

	return nil
}

// logPacketCounts periodically logs TUN packet counters for diagnostics.
func (e *Engine) logPacketCounts() {
	defer e.wg.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-e.closeCh:
			return
		case <-ticker.C:
			util.LogInfo("tun counters: read=%d write=%d", e.readPackets.Load(), e.writePackets.Load())
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
				util.LogInfo("tun read FAKE: %s -> %s (proto=%d len=%d cnt=%d)", srcIP, dstIP, ipProto, n, e.readPackets.Load())
			} else if e.readPackets.Load() <= 200 {
				util.LogInfo("tun read: %s -> %s (proto=%d len=%d)", srcIP, dstIP.String(), ipProto, n)
			}
		}

		// Mesh interception: let mesh layer decide if packet should be routed via mesh.
		// The mesh interceptor checks its routing table (including gateway routes) to determine
		// if the packet should be sent via mesh or handled normally.
		if e.meshInterceptor != nil && proto == ipv4.ProtocolNumber && n >= 20 {
			dstIP := net.IP(readBuf[16:20])
			pktBuf := make([]byte, n)
			copy(pktBuf, readBuf[:n])
			if e.meshInterceptor(dstIP, pktBuf) {
				continue
			}
		}

		pktBuf := make([]byte, n)
		copy(pktBuf, readBuf[:n])

		// NAT: replace src IP with VIP for non-mesh IPv4 packets.
		// This marks TUN-originated traffic so return traffic (dst=VIP) can be
		// routed back through outNIC → TUN instead of loopback.
		if e.meshSubnet != nil && n >= 20 && pktBuf[0]>>4 == 4 {
			srcIP := net.IP(pktBuf[12:16])
			if !e.meshSubnet.Contains(srcIP) {
				vip := e.meshSubnet.IP.To4()
				vipAddr := net.IP{vip[0], vip[1], vip[2], vip[3] + 1}
				oldSrc := make([]byte, 4)
				copy(oldSrc, pktBuf[12:16])
				newSrc := vipAddr.To4()

				// Update IP header checksum
				var oldCsum uint32
				oldCsum += uint32(oldSrc[0])<<8 | uint32(oldSrc[1])
				oldCsum += uint32(oldSrc[2])<<8 | uint32(oldSrc[3])
				var newCsum uint32
				newCsum += uint32(newSrc[0])<<8 | uint32(newSrc[1])
				newCsum += uint32(newSrc[2])<<8 | uint32(newSrc[3])

				ipHdr := binary.BigEndian.Uint16(pktBuf[10:12])
				diff := newCsum - oldCsum
				csum := uint32(^ipHdr)
				csum += diff
				for csum > 0xffff {
					csum = (csum >> 16) + (csum & 0xffff)
				}
				binary.BigEndian.PutUint16(pktBuf[10:12], ^uint16(csum))

				// Update TCP/UDP checksum using pseudo-header diff
				headerLen := int(pktBuf[0]&0x0f) * 4
				proto := pktBuf[9]
				if (proto == 6 || proto == 17) && n >= headerLen+4 {
					var transportCsum uint32
					transportCsum += uint32(oldSrc[0])<<8 | uint32(oldSrc[1])
					transportCsum += uint32(oldSrc[2])<<8 | uint32(oldSrc[3])
					var transportNewCsum uint32
					transportNewCsum += uint32(newSrc[0])<<8 | uint32(newSrc[1])
					transportNewCsum += uint32(newSrc[2])<<8 | uint32(newSrc[3])
					transportDiff := transportNewCsum - transportCsum

					var csumOffset int
					if proto == 6 {
						csumOffset = headerLen + 16
					} else {
						csumOffset = headerLen + 6
					}
					if n >= csumOffset+2 {
						oldTransport := binary.BigEndian.Uint16(pktBuf[csumOffset : csumOffset+2])
						if oldTransport != 0 {
							tc := uint32(^oldTransport)
							tc += transportDiff
							for tc > 0xffff {
								tc = (tc >> 16) + (tc & 0xffff)
							}
							binary.BigEndian.PutUint16(pktBuf[csumOffset:csumOffset+2], ^uint16(tc))
						}
					}
				}

				// Replace src IP
				copy(pktBuf[12:16], newSrc)
			}
		}

		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(pktBuf)})
		e.linkEP.InjectInbound(proto, pkt)
		pkt.DecRef()
	}
}

// writeLoop reads outbound packets from netstack and dispatches them.
// With TUN: writes to the TUN device for OS delivery.
// Without TUN: mesh-intercepted packets go via mesh, others are dropped.
func (e *Engine) writeLoop() {
	defer e.wg.Done()
	for {
		select {
		case <-e.closeCh:
			return
		default:
		}

		pkt := e.linkEP.Read()
		if pkt == nil {
			select {
			case <-e.closeCh:
				return
			case <-time.After(10 * time.Millisecond):
			}
			continue
		}

		buf := pkt.ToBuffer()
		data := buf.Flatten()

		// Mesh interception: route packets destined for remote mesh nodes via mesh.
		// Skip packets destined for our own mesh VIP — those are replies from netstack
		// (e.g. ICMP echo replies) that should be delivered to the OS via the TUN device.
		if e.meshInterceptor != nil && len(data) >= 20 && (data[0]>>4) == 4 {
			pktDst := net.IP(data[16:20])
			isLocal := e.isLocalMeshVIP(pktDst)
			if !isLocal {
				pktBuf := make([]byte, len(data))
				copy(pktBuf, data)
				if e.meshInterceptor(pktDst, pktBuf) {
					pkt.DecRef()
					continue
				}
			}
		}

		// No TUN device — drop non-mesh packets
		if e.device == nil {
			pkt.DecRef()
			continue
		}

		// Reverse NAT: if dst = VIP, replace with hostIP so the OS can match
		// the return packet to the original connection.
		if e.meshSubnet != nil && len(data) >= 20 && (data[0]>>4) == 4 {
			vip := e.meshSubnet.IP.To4()
			vipAddr := net.IP{vip[0], vip[1], vip[2], vip[3] + 1}
			dstIP := net.IP(data[16:20])
			if dstIP.Equal(vipAddr) {
				hostIP := net.IP(e.addr.AsSlice()).To4()
				oldDst := make([]byte, 4)
				copy(oldDst, data[16:20])
				newDst := hostIP

				// Make a mutable copy for modification
				natData := make([]byte, len(data))
				copy(natData, data)

				// Update IP header checksum
				var oldCsum uint32
				oldCsum += uint32(oldDst[0])<<8 | uint32(oldDst[1])
				oldCsum += uint32(oldDst[2])<<8 | uint32(oldDst[3])
				var newCsum uint32
				newCsum += uint32(newDst[0])<<8 | uint32(newDst[1])
				newCsum += uint32(newDst[2])<<8 | uint32(newDst[3])

				ipHdr := binary.BigEndian.Uint16(natData[10:12])
				diff := newCsum - oldCsum
				csum := uint32(^ipHdr)
				csum += diff
				for csum > 0xffff {
					csum = (csum >> 16) + (csum & 0xffff)
				}
				binary.BigEndian.PutUint16(natData[10:12], ^uint16(csum))

				// Update TCP/UDP checksum using pseudo-header diff
				headerLen := int(natData[0]&0x0f) * 4
				proto := natData[9]
				if (proto == 6 || proto == 17) && len(natData) >= headerLen+4 {
					var transportCsum uint32
					transportCsum += uint32(oldDst[0])<<8 | uint32(oldDst[1])
					transportCsum += uint32(oldDst[2])<<8 | uint32(oldDst[3])
					var transportNewCsum uint32
					transportNewCsum += uint32(newDst[0])<<8 | uint32(newDst[1])
					transportNewCsum += uint32(newDst[2])<<8 | uint32(newDst[3])
					transportDiff := transportNewCsum - transportCsum

					var csumOffset int
					if proto == 6 {
						csumOffset = headerLen + 16
					} else {
						csumOffset = headerLen + 6
					}
					if len(natData) >= csumOffset+2 {
						oldTransport := binary.BigEndian.Uint16(natData[csumOffset : csumOffset+2])
						if oldTransport != 0 {
							tc := uint32(^oldTransport)
							tc += transportDiff
							for tc > 0xffff {
								tc = (tc >> 16) + (tc & 0xffff)
							}
							binary.BigEndian.PutUint16(natData[csumOffset:csumOffset+2], ^uint16(tc))
						}
					}
				}

				// Replace dst IP
				copy(natData[16:20], newDst)
				data = natData
			}
		}

		// Log outbound packets for debugging. Fake-IP replies and packets to the
		// TUN host IP are always logged; everything else is logged for the first
		// 200 packets to cap noise.
		if len(data) >= 20 && (data[0]>>4) == 4 {
			dstIP := net.IP(data[16:20])
			srcIP := net.IP(data[12:16])
			ipProto := data[9]
			isFake := (e.meshSubnet != nil) && (e.meshSubnet.Contains(dstIP) || e.meshSubnet.Contains(srcIP))
			if isFake {
				util.LogInfo("tun write FAKE: %s -> %s (proto=%d len=%d)", srcIP, dstIP, ipProto, len(data))
			} else if e.writePackets.Load() <= 200 {
				util.LogInfo("tun write: %s -> %s (proto=%d len=%d)", srcIP, dstIP, ipProto, len(data))
			}
		}

		if _, err := e.device.Write(data); err != nil {
			select {
			case <-e.closeCh:
				pkt.DecRef()
				return
			default:
				util.LogWarn("tun: write error: %v", err)
			}
		} else {
			e.writePackets.Add(1)
			e.notifyStatsChanged()
		}
		pkt.DecRef()
	}
}

// acceptTCP accepts TCP connections from netstack and proxies them.
func (e *Engine) acceptTCP() {
	defer e.wg.Done()

	fwd := tcp.NewForwarder(e.ns, 0, 1024, func(r *tcp.ForwarderRequest) {
		var wq waiter.Queue
		ep, err := r.CreateEndpoint(&wq)
		if err != nil {
			util.LogWarn("tun: tcp CreateEndpoint fail: %v", err)
			r.Complete(true)
			return
		}
		id := r.ID()
		r.Complete(false)
		defer ep.Close()

		conn := gonet.NewTCPConn(&wq, ep)
		defer conn.Close()

		dstAddr := net.IP(id.LocalAddress.AsSlice()).String()
		dstPort := int(id.LocalPort)

		e.handleConn(conn, dstAddr, dstPort)
	})

	e.ns.SetTransportProtocolHandler(tcp.ProtocolNumber, fwd.HandlePacket)

	<-e.closeCh
}

// relayWithIdleTimeout bidirectionally copies data between conn and target.
// A watchdog goroutine monitors activity via atomic timestamps and calls Close()
// when no data flows for idleTimeout. This avoids gVisor's SetReadDeadline lock
// contention that previously caused CreateEndpoint timeouts.
func relayWithIdleTimeout(conn, target net.Conn, idleTimeout time.Duration) {
	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())

	done := make(chan struct{})
	defer close(done)

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if time.Since(time.Unix(0, lastActivity.Load())) > idleTimeout {
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

// acceptUDP accepts UDP datagrams from netstack and proxies them through
// the proxy chain or direct dial, mirroring the TCP acceptTCP pattern.
func (e *Engine) acceptUDP() {
	defer e.wg.Done()

	fwd := udp.NewForwarder(e.ns, func(r *udp.ForwarderRequest) {
		go func() {
			var wq waiter.Queue
			ep, err := r.CreateEndpoint(&wq)
			if err != nil {
				return
			}
			defer ep.Close()

			id := r.ID()
			dstAddr := net.IP(id.LocalAddress.AsSlice()).String()
			dstPort := int(id.LocalPort)

			conn := gonet.NewUDPConn(&wq, ep)
			defer conn.Close()

			e.handleUDP(conn, dstAddr, dstPort)
		}()
	})

	e.ns.SetTransportProtocolHandler(udp.ProtocolNumber, fwd.HandlePacket)

	<-e.closeCh
}

// handleUDP relays UDP datagrams between netstack and the real network via proxy or direct.
// It preserves datagram boundaries by reading/writing one datagram at a time.
func (e *Engine) handleUDP(netstackConn net.Conn, dstAddr string, dstPort int) {
	// Check if this is a Fake-IP: restore original domain.
	var domain string
	if d := e.fakeIP.LookupDomain(dstAddr); d != "" {
		domain = d
		util.LogInfo("tun: udp fake-ip %s -> %s", dstAddr, domain)
	}

	connID := util.NextConnID()

	// Use domain for rule matching (so domain-based rules work).
	matchAddr := dstAddr
	if domain != "" {
		matchAddr = domain
	}

	req := config.NewConnectRequest(matchAddr, dstPort)
	var proxy *config.Proxy
	var matchResult *config.MatchResult
	if e.ruleConf != nil {
		req = e.ruleConf.Resolving(req)
		proxy, matchResult = e.ruleConf.Match(req, TUNMapping)
	}

	resolvedAddr := req.DstAddr
	resolvedPort := req.DstPort

	if proxy != nil && strings.ToUpper(proxy.Type) == config.ProxyREJECT {
		util.LogInfo("[TUN] [%s] udp %s:%d -> REJECTED", connID, resolvedAddr, resolvedPort)
		connlog.Log("TUN", "UDP", "", matchAddr, resolvedAddr, resolvedPort, matchResult, "reject", nil)
		return
	}

	var targetConn net.PacketConn
	var err error
	var dialIP net.IP

	if proxy != nil && strings.ToUpper(proxy.Type) != config.ProxyDIRECT {
		targetConn, err = dialer.ChainUDPDial(proxy)
		if err != nil {
			util.LogWarn("[TUN] [%s] udp dial %s:%d via %s fail: %v", connID, resolvedAddr, resolvedPort, proxy.Name, err)
			connlog.Log("TUN", "UDP", "", matchAddr, resolvedAddr, resolvedPort, matchResult, "fail", err)
			return
		}
		dialIP = net.ParseIP(resolvedAddr)
	} else {
		// Direct dial: resolve real IP now if we have a domain.
		dialIP = net.ParseIP(resolvedAddr)
		if domain != "" {
			// Resolve the real IP for DIRECT connections.
			ips, err := e.resolveForDirect(domain)
			if err != nil || len(ips) == 0 {
				util.LogWarn("[TUN] [%s] udp resolve %s fail: %v", connID, domain, err)
				connlog.Log("TUN", "UDP", "", matchAddr, domain, resolvedPort, &config.MatchResult{ProxyName: "DIRECT"}, "fail", err)
				return
			}
			// Prefer IPv4
			for _, ip := range ips {
				if ip4 := ip.To4(); ip4 != nil {
					dialIP = ip4
					break
				}
			}
			util.LogInfo("[TUN] [%s] udp resolved %s -> %s for DIRECT", connID, domain, dialIP)
		}
		targetConn, err = dialer.ListenPacketBoundTo("udp", "", dialIP)
		if err != nil {
			util.LogWarn("[TUN] [%s] udp direct dial %s:%d fail: %v", connID, resolvedAddr, resolvedPort, err)
			connlog.Log("TUN", "UDP", "", matchAddr, dialIP.String(), resolvedPort, &config.MatchResult{ProxyName: "DIRECT"}, "fail", err)
			return
		}
	}
	defer targetConn.Close()

	// Use the resolved IP for the destination address.
	dstUDPAddr := &net.UDPAddr{IP: dialIP, Port: resolvedPort}
	util.LogInfo("[TUN] [%s] udp %s:%d -> %s", connID, resolvedAddr, resolvedPort, proxyDesc(proxy))
	if proxy == nil || strings.EqualFold(proxy.Type, config.ProxyDIRECT) {
		// Preserve Rule and TimeRange from original matchResult if available
		if matchResult != nil {
			matchResult.ProxyName = "DIRECT"
		} else {
			matchResult = &config.MatchResult{ProxyName: "DIRECT"}
		}
	} else if matchResult != nil && proxy.Name != matchResult.ProxyName {
		// Set actual proxy name when different from rule (e.g., group resolution)
		matchResult.ActualProxy = proxy.Name
	}
	connlog.Log("TUN", "UDP", "", matchAddr, resolvedAddr, resolvedPort, matchResult, "ok", nil)
	connlog.TrackActive(connID, "TUN", "UDP", "", matchAddr, resolvedAddr, resolvedPort, matchResult)
	defer connlog.RemoveActive(connID)

	relayUDP(netstackConn, targetConn, dstUDPAddr)
}

// relayUDP copies datagrams between the netstack UDP connection and the target
// PacketConn, preserving datagram boundaries. Both sides use a 30-second idle
// timeout to prevent goroutine leaks when the remote stops responding.
func relayUDP(netstackConn net.Conn, targetConn net.PacketConn, dstAddr *net.UDPAddr) {
	const bufSize = 65535
	const idleTimeout = 30 * time.Second

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, bufSize)
		for {
			_ = netstackConn.SetReadDeadline(time.Now().Add(idleTimeout))
			n, err := netstackConn.Read(buf)
			if err != nil {
				return
			}
			if _, err := targetConn.WriteTo(buf[:n], dstAddr); err != nil {
				return
			}
		}
	}()

	buf := make([]byte, bufSize)
	for {
		_ = targetConn.SetReadDeadline(time.Now().Add(idleTimeout))
		n, _, err := targetConn.ReadFrom(buf)
		if err != nil {
			break
		}
		if _, err := netstackConn.Write(buf[:n]); err != nil {
			break
		}
	}
	wg.Wait()
}

// handleConn routes a TUN-side TCP connection through the proxy chain or direct.
func (e *Engine) handleConn(conn net.Conn, dstAddr string, dstPort int) {
	defer conn.Close()

	// Check if this is a Fake-IP: restore original domain.
	var domain string
	if d := e.fakeIP.LookupDomain(dstAddr); d != "" {
		domain = d
		util.LogInfo("tun: fake-ip %s -> %s", dstAddr, domain)
	}

	connID := util.NextConnID()

	// Use domain for rule matching (so domain-based rules work).
	matchAddr := dstAddr
	if domain != "" {
		matchAddr = domain
	}

	req := config.NewConnectRequest(matchAddr, dstPort)
	var proxy *config.Proxy
	var matchResult *config.MatchResult
	if e.ruleConf != nil {
		req = e.ruleConf.Resolving(req)
		proxy, matchResult = e.ruleConf.Match(req, TUNMapping)
	}

	// Use the (possibly redirected) destination from Resolving
	resolvedAddr := req.DstAddr
	resolvedPort := req.DstPort

	var targetConn net.Conn
	var err error

	if proxy != nil && strings.ToUpper(proxy.Type) == config.ProxyREJECT {
		util.LogInfo("[TUN] [%s] %s:%d -> REJECTED", connID, resolvedAddr, resolvedPort)
		connlog.Log("TUN", "TCP", "", matchAddr, resolvedAddr, resolvedPort, matchResult, "reject", nil)
		return
	}

	if proxy != nil && strings.ToUpper(proxy.Type) != config.ProxyDIRECT {
		targetConn, err = dialer.ChainDialWithID(proxy, resolvedAddr, resolvedPort, connID)
		if err != nil {
			util.LogWarn("[TUN] [%s] dial %s:%d via %s fail: %v", connID, resolvedAddr, resolvedPort, proxy.Name, err)
			connlog.Log("TUN", "TCP", "", matchAddr, resolvedAddr, resolvedPort, matchResult, "fail", err)
			return
		}
	} else {
		// Direct dial: resolve real IP now if we have a domain.
		dialAddr := resolvedAddr
		if domain != "" {
			// Resolve the real IP for DIRECT connections.
			ips, err := e.resolveForDirect(domain)
			if err != nil || len(ips) == 0 {
				util.LogWarn("[TUN] [%s] resolve %s fail: %v", connID, domain, err)
				connlog.Log("TUN", "TCP", "", matchAddr, domain, resolvedPort, &config.MatchResult{ProxyName: "DIRECT"}, "fail", err)
				return
			}
			// Prefer IPv4
			for _, ip := range ips {
				if ip4 := ip.To4(); ip4 != nil {
					dialAddr = ip4.String()
					break
				}
			}
			util.LogInfo("[TUN] [%s] resolved %s -> %s for DIRECT", connID, domain, dialAddr)
		}
		targetConn, err = dialer.DialRouteAware("tcp", net.JoinHostPort(dialAddr, fmt.Sprintf("%d", resolvedPort)))
		if err != nil {
			util.LogWarn("[TUN] [%s] direct dial %s:%d fail: %v", connID, dialAddr, resolvedPort, err)
			connlog.Log("TUN", "TCP", "", matchAddr, dialAddr, resolvedPort, &config.MatchResult{ProxyName: "DIRECT"}, "fail", err)
			return
		}
	}
	defer targetConn.Close()

	util.LogInfo("[TUN] [%s] %s:%d -> %s", connID, resolvedAddr, resolvedPort, proxyDesc(proxy))
	if proxy == nil || strings.EqualFold(proxy.Type, config.ProxyDIRECT) {
		// Preserve Rule and TimeRange from original matchResult if available
		if matchResult != nil {
			matchResult.ProxyName = "DIRECT"
		} else {
			matchResult = &config.MatchResult{ProxyName: "DIRECT"}
		}
	} else if matchResult != nil && proxy.Name != matchResult.ProxyName {
		// Set actual proxy name when different from rule (e.g., group resolution)
		matchResult.ActualProxy = proxy.Name
	}
	connlog.Log("TUN", "TCP", "", matchAddr, resolvedAddr, resolvedPort, matchResult, "ok", nil)
	connlog.TrackActive(connID, "TUN", "TCP", "", matchAddr, resolvedAddr, resolvedPort, matchResult)
	defer connlog.RemoveActive(connID)
	relayWithIdleTimeout(conn, targetConn, 5*time.Minute)
}

func proxyDesc(p *config.Proxy) string {
	if p == nil || strings.EqualFold(p.Type, config.ProxyDIRECT) {
		return "DIRECT"
	}
	return p.Name
}

// queryInternalDNS sends a raw DNS query to the TUN DNS hijacker and returns
// the raw response bytes. On Windows it bypasses the gVisor netstack because
// locally-originated UDP packets to loopback/TUN-subnet addresses are not
// reliably delivered back to the same process; on other platforms it uses the
// internal gVisor UDP path.
func (e *Engine) queryInternalDNS(query []byte) ([]byte, error) {
	if !e.IsEnabled() || e.ns == nil {
		return nil, fmt.Errorf("engine disabled or no netstack")
	}

	if e.dnsHijack != nil {
		if resp, err := e.dnsHijack.Resolve(query); err == nil {
			return resp, nil
		}
		// Fall back to netstack path if direct resolve fails.
	}

	remoteAddr := tcpip.FullAddress{NIC: outNICID, Addr: e.dnsAddr, Port: 53}
	conn, err := gonet.DialUDP(e.ns, nil, &remoteAddr, ipv4.ProtocolNumber)
	if err != nil {
		return nil, fmt.Errorf("dial internal dns fail: %v", err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return nil, fmt.Errorf("set deadline fail: %v", err)
	}

	if _, err := conn.Write(query); err != nil {
		return nil, fmt.Errorf("write fail: %v", err)
	}

	resp := make([]byte, 512)
	n, err := conn.Read(resp)
	if err != nil {
		return nil, fmt.Errorf("read fail: %v", err)
	}
	return resp[:n], nil
}
