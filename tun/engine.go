package tun

import (
	"bytes"
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
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
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

// logTCPPacket logs TCP packet details for debugging
func logTCPPacket(prefix string, data []byte) {
	if len(data) < 20 {
		return
	}
	srcIP := net.IP(data[12:16])
	dstIP := net.IP(data[16:20])
	headerLen := int(data[0]&0x0f) * 4
	if len(data) < headerLen+20 {
		util.LogDebug("%s %s -> %s (TCP header too short)", prefix, srcIP, dstIP)
		return
	}
	srcPort := uint16(data[headerLen])<<8 | uint16(data[headerLen+1])
	dstPort := uint16(data[headerLen+2])<<8 | uint16(data[headerLen+3])
	seq := uint32(data[headerLen+4])<<24 | uint32(data[headerLen+5])<<16 | uint32(data[headerLen+6])<<8 | uint32(data[headerLen+7])
	ack := uint32(data[headerLen+8])<<24 | uint32(data[headerLen+9])<<16 | uint32(data[headerLen+10])<<8 | uint32(data[headerLen+11])
	flags := data[headerLen+13]
	flagStr := ""
	if flags&0x02 != 0 {
		flagStr += "SYN "
	}
	if flags&0x10 != 0 {
		flagStr += "ACK "
	}
	if flags&0x01 != 0 {
		flagStr += "FIN "
	}
	if flags&0x04 != 0 {
		flagStr += "RST "
	}
	util.LogDebug("%s %s:%d -> %s:%d [%s] seq=%d ack=%d len=%d",
		prefix, srcIP, srcPort, dstIP, dstPort, flagStr, seq, ack, len(data))
}

// Engine manages the TUN device, netstack, and traffic interception.
type Engine struct {
	ruleConf   *config.RuleConfiguration
	device     Device
	linkEP     *channel.Endpoint
	ns         *stack.Stack
	fakeIP    *mesh.FakeIPPool
	dnsHijack *mesh.DNSHijacker
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
	e.meshOutboundCh = make(chan meshOutboundPacket, 16384)
	e.tunWG.Add(1)
	go e.meshOutboundLoop()
	util.LogDebug("tun: mesh interceptor set (localVIPs=%v)", localVIPs)
}

// SetNATTable sets the shared NAT table for TUN source NAT and reverse NAT.
func (e *Engine) SetNATTable(nat *mesh.NATTable) {
	e.natTable = nat
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
	if e.dnsHijack != nil {
		e.dnsHijack.SetDomainResolver(resolver)
		util.LogDebug("tun: DNS domain resolver set")
	}
}

// SetDNSHijacker binds the mesh's DNS hijacker to this engine's netstack.
// Called when mesh is configured and TUN engine is started.
func (e *Engine) SetDNSHijacker(h *mesh.DNSHijacker, fakeIP *mesh.FakeIPPool) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if !e.running {
		return fmt.Errorf("tun: engine not running")
	}

	// Bind netstack to the hijacker
	h.BindNetstack(e.ns, e.addr, e.dnsAddr)

	// Start the hijacker
	if err := h.Start(&e.wg); err != nil {
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

// ResolveDomain resolves a domain name by sending a DNS query through the
// netstack UDP socket to the local hijacker (dnsAddr:53). The packet goes
// through the loopback NIC and is handled by the hijacker, which allocates
// a fakeIP from the local pool or forwards to a remote mesh gateway.
func (e *Engine) ResolveDomain(domain string) (net.IP, error) {
	util.LogDebug("netstack: ResolveDomain called for domain=%s", domain)
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
	query := mesh.BuildDNSQuery(domain, txID)
	if _, err := ep.Write(&mesh.SlicePayload{Data: query}, tcpip.WriteOptions{}); err != nil {
		return nil, fmt.Errorf("resolve %s: write: %v", domain, err)
	}

	select {
	case <-ch:
	case <-time.After(30 * time.Second):
		return nil, fmt.Errorf("resolve %s: timeout", domain)
	}

	var buf bytes.Buffer
	if _, err := ep.Read(&buf, tcpip.ReadOptions{}); err != nil {
		return nil, fmt.Errorf("resolve %s: read: %v", domain, err)
	}
	fakeIP, _ := mesh.ParseDNSResponseIP(buf.Bytes())
	if fakeIP == nil {
		return nil, fmt.Errorf("resolve %s: bad response", domain)
	}
	return fakeIP, nil
}

// NetDial dials a connection through the netstack. For addresses in the fakeIP
// or mesh subnet, the connection goes through the loopback NIC and is caught by
// TCP/UDP forwarders, which route to local or remote destinations transparently.
func (e *Engine) NetDial(network, addr string) (net.Conn, error) {
	util.LogDebug("netstack: NetDial called with network=%s addr=%s", network, addr)
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

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	switch network {
	case "tcp", "tcp4":
		util.LogDebug("netstack: DialContextTCP to %s:%d", host, portNum)
		// Use custom dial that allows pre-connect callback for ModeBTable registration
		conn, err := e.dialTCPWithPreConnect(ctx, ns, remoteAddr)
		if err != nil {
			util.LogWarn("netstack: DialContextTCP failed: %v", err)
			return nil, err
		}
		util.LogDebug("netstack: DialContextTCP succeeded to %s:%d", host, portNum)
		return conn, nil
	case "udp", "udp4":
		return gonet.DialUDP(ns, nil, &remoteAddr, ipv4.ProtocolNumber)
	default:
		return nil, fmt.Errorf("netstack dial: unsupported network: %s", network)
	}
}

// dialTCPWithPreConnect creates a TCP connection with a pre-connect callback.
// The callback is called after bind (port allocated) but before connect (SYN sent),
// allowing ModeBTable registration before the forwarder is triggered.
func (e *Engine) dialTCPWithPreConnect(ctx context.Context, s *stack.Stack, remoteAddr tcpip.FullAddress) (net.Conn, error) {
	var wq waiter.Queue
	ep, tcpErr := s.NewEndpoint(tcp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
	if tcpErr != nil {
		return nil, fmt.Errorf("create endpoint: %s", tcpErr)
	}

	// Bind to GIP (dnsAddr) with port 0 to allocate a source port
	localAddr := tcpip.FullAddress{
		Addr: e.dnsAddr,
		Port: 0, // Let gVisor allocate the port
	}
	if tcpErr := ep.Bind(localAddr); tcpErr != nil {
		ep.Close()
		return nil, fmt.Errorf("bind: %s", tcpErr)
	}

	// Get the allocated source port
	localAddr, tcpErr = ep.GetLocalAddress()
	if tcpErr != nil {
		ep.Close()
		return nil, fmt.Errorf("get local address: %s", tcpErr)
	}
	srcPort := localAddr.Port

	// Call pre-connect callback if set (for ModeBTable registration)
	if e.preConnectCallback != nil {
		e.preConnectCallback(remoteAddr.Addr, remoteAddr.Port, srcPort)
	}

	// Create wait queue entry for connect completion
	waitEntry, notifyCh := waiter.NewChannelEntry(waiter.WritableEvents)
	wq.EventRegister(&waitEntry)
	defer wq.EventUnregister(&waitEntry)

	select {
	case <-ctx.Done():
		ep.Close()
		return nil, ctx.Err()
	default:
	}

	// Now connect (sends SYN)
	tcpErr = ep.Connect(remoteAddr)
	if _, ok := tcpErr.(*tcpip.ErrConnectStarted); ok {
		select {
		case <-ctx.Done():
			ep.Close()
			return nil, ctx.Err()
		case <-notifyCh:
		}
		tcpErr = ep.LastError()
	}
	if tcpErr != nil {
		ep.Close()
		return nil, &net.OpError{
			Op:   "connect",
			Net:  "tcp",
			Addr: fullToTCPAddr(remoteAddr),
			Err:  fmt.Errorf("%s", tcpErr),
		}
	}

	return gonet.NewTCPConn(&wq, ep), nil
}

// NetDialWithModeB dials through the netstack and registers in ModeBTable before sending SYN.
// This ensures the forwarder can find the entry for local loopback cases.
func (e *Engine) NetDialWithModeB(network, addr string, clientAddr string, inbound string, mapping *config.Mapping) (net.Conn, error) {
	util.LogDebug("netstack: NetDialWithModeB called with network=%s addr=%s client=%s", network, addr, clientAddr)
	e.mu.Lock()
	running := e.running
	ns := e.ns
	modeBTable := e.modeBTable
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

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	if network != "tcp" && network != "tcp4" {
		return nil, fmt.Errorf("netstack dial: unsupported network: %s", network)
	}

	// Create endpoint and bind
	var wq waiter.Queue
	ep, tcpErr := ns.NewEndpoint(tcp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
	if tcpErr != nil {
		return nil, fmt.Errorf("create endpoint: %s", tcpErr)
	}

	// Bind to GIP with port 0 to allocate source port
	localAddr := tcpip.FullAddress{
		Addr: e.dnsAddr,
		Port: 0,
	}
	if tcpErr := ep.Bind(localAddr); tcpErr != nil {
		ep.Close()
		return nil, fmt.Errorf("bind: %s", tcpErr)
	}

	// Get allocated source port
	localAddr, tcpErr = ep.GetLocalAddress()
	if tcpErr != nil {
		ep.Close()
		return nil, fmt.Errorf("get local address: %s", tcpErr)
	}
	srcPort := localAddr.Port

	// Register in ModeBTable BEFORE connect (before SYN is sent)
	if modeBTable != nil {
		dstKey := net.JoinHostPort(host, port)
		modeBTable.Register(6, dstKey, srcPort, clientAddr, inbound, mapping)
		util.LogDebug("netstack: registered ModeBTable before SYN: dst=%s srcPort=%d client=%s", dstKey, srcPort, clientAddr)
	}

	// Create wait queue entry for connect completion
	waitEntry, notifyCh := waiter.NewChannelEntry(waiter.WritableEvents)
	wq.EventRegister(&waitEntry)
	defer wq.EventUnregister(&waitEntry)

	select {
	case <-ctx.Done():
		ep.Close()
		return nil, ctx.Err()
	default:
	}

	// Now connect (sends SYN)
	tcpErr = ep.Connect(remoteAddr)
	if _, ok := tcpErr.(*tcpip.ErrConnectStarted); ok {
		select {
		case <-ctx.Done():
			ep.Close()
			return nil, ctx.Err()
		case <-notifyCh:
		}
		tcpErr = ep.LastError()
	}
	if tcpErr != nil {
		// Unregister on connect failure
		if modeBTable != nil {
			dstKey := net.JoinHostPort(host, port)
			modeBTable.Unregister(6, dstKey, srcPort)
		}
		ep.Close()
		return nil, &net.OpError{
			Op:   "connect",
			Net:  "tcp",
			Addr: fullToTCPAddr(remoteAddr),
			Err:  fmt.Errorf("%s", tcpErr),
		}
	}

	util.LogDebug("netstack: NetDialWithModeB succeeded to %s:%d srcPort=%d", host, portNum, srcPort)
	return gonet.NewTCPConn(&wq, ep), nil
}

// fullToTCPAddr converts a tcpip.FullAddress to a net.TCPAddr
func fullToTCPAddr(addr tcpip.FullAddress) *net.TCPAddr {
	return &net.TCPAddr{
		IP:   net.IP(addr.Addr.AsSlice()),
		Port: int(addr.Port),
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

	util.LogDebug("tun: mesh addresses: hostIP=%s GIP=%s", hostIP, gip)
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
		if len(data) >= 20 && data[9] == 6 {
			logTCPPacket("[TCP-DEBUG] InjectMeshPacket:", data)
			headerLen := int(data[0]&0x0f) * 4
			if len(data) >= headerLen+14 {
				flags := data[headerLen+13]
				isSYN := (flags&0x02) != 0 && (flags&0x10) == 0
				if isSYN {
					dstPort := uint16(data[headerLen+2])<<8 | uint16(data[headerLen+3])
					util.LogInfo("[TCP-DIAG] InjectMeshPacket SYN: src=%s dst=%s:%d len=%d meshSrc=%v meshDst=%v",
						srcIP, dstIP, dstPort, len(data), e.isMeshIP(srcIP), e.isMeshIP(dstIP))
				}
			}
		}
	}

	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(data),
	})
	e.linkEP.InjectInbound(proto, pkt)
	pkt.DecRef()
	return nil
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
			running := e.running
			e.mu.Unlock()
			if !running || dev == nil {
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
	running := e.running
	e.mu.Unlock()

	if !running {
		return fmt.Errorf("TUN not ready (running=%v)", running)
	}
	if len(data) >= 20 {
		srcIP := net.IP(data[12:16])
		dstIP := net.IP(data[16:20])
		util.LogDebug("tun: WriteMeshPacket %s -> %s len=%d", srcIP, dstIP, len(data))
		if data[9] == 6 {
			logTCPPacket("[TCP-DEBUG] WriteMeshPacket:", data)
		}
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
	if err := e.ns.AddProtocolAddress(1, protoAddr, stack.AddressProperties{}); err != nil {
		return fmt.Errorf("add mesh VIP %s: %v", vip, err)
	}
	util.LogDebug("tun: registered mesh VIP %s with netstack", vip)
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
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return fmt.Errorf("stack already running")
	}

	// Address determination (always needed for netstack)
	// ConfigureMeshAddresses MUST be called before Start() to set mesh-derived addresses.
	// hostIP is the address assigned to the TUN adapter (OS side);
	// it must NOT be added as a local netstack address, otherwise replies
	// destined to it from the DNS hijacker / forwarders would be looped back
	// inside netstack instead of being written back to the TUN device.
	// dnsIP is a dedicated DNS address within the TUN subnet. DNSHijacker binds
	// to this address inside netstack. DNS queries are routed through the TUN
	// device to reach it, eliminating the need for a host-side DNS proxy.
	if e.addr == (tcpip.Address{}) || e.dnsAddr == (tcpip.Address{}) {
		e.mu.Unlock()
		return fmt.Errorf("tun: mesh addresses not configured (ConfigureMeshAddresses must be called before Start)")
	}

	// gVisor netstack
	if err := e.initStack(); err != nil {
		e.mu.Unlock()
		return fmt.Errorf("tun: init netstack: %w", err)
	}

	// FakeIPPool and DNSHijacker are now managed by mesh.
	// They will be bound via SetDNSHijacker() if mesh is configured.

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
	util.LogDebug("gVisor netstack started")
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
	e.meshWriteCh = make(chan []byte, 8192)
	e.mu.Unlock()

	e.tunWG.Add(1)
	go e.meshWriteLoop()

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
	e.mu.Lock()
	if !e.running {
		e.mu.Unlock()
		return nil
	}
	e.running = false
	close(e.closeCh)

	// Stop services before waiting for goroutines, since service goroutines
	// (e.g. DNS hijacker) are part of wg and need their endpoints closed to exit.
	if e.dnsHijack != nil {
		e.dnsHijack.Stop()
	}
	e.mu.Unlock()

	// Wait for stack goroutines to finish
	e.wg.Wait()

	if e.ns != nil {
		e.ns.Close()
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

// initStack creates the gvisor netstack with a single NIC.
// All traffic (TUN, DNS hijacker, TCP forwarder, Mode B sockets) shares one NIC.
// writeLoop handles all routing decisions: VIP/hostIP → TUN, mesh → mesh link, other → re-inject.
func (e *Engine) initStack() error {
	linkEP := channel.New(8192, 1500, "")
	e.linkEP = linkEP

	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	e.ns = s

	if err := s.CreateNIC(1, linkEP); err != nil {
		return fmt.Errorf("create nic: %v", err)
	}

	ap := tcpip.AddressWithPrefix{Address: e.dnsAddr, PrefixLen: 32}
	if err := s.AddProtocolAddress(1, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: ap,
	}, stack.AddressProperties{}); err != nil {
		return fmt.Errorf("add dns address: %v", err)
	}

	s.SetPromiscuousMode(1, true)
	s.SetSpoofing(1, true)
	_ = s.SetForwardingDefaultAndAllNICs(ipv4.ProtocolNumber, true)
	_ = s.SetForwardingDefaultAndAllNICs(ipv6.ProtocolNumber, true)

	routes := []tcpip.Route{
		{Destination: header.IPv4EmptySubnet, NIC: 1},
		{Destination: header.IPv6EmptySubnet, NIC: 1},
	}
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
			util.LogDebug("tun counters: read=%d write=%d", e.readPackets.Load(), e.writePackets.Load())
		}
	}
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
				newPkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
					Payload: buffer.MakeWithData(pkt.data),
				})
				e.linkEP.InjectInbound(ipv4.ProtocolNumber, newPkt)
				newPkt.DecRef()
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

		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(pktBuf)})
		e.linkEP.InjectInbound(proto, pkt)
		pkt.DecRef()
	}
}

// writeLoop reads outbound packets from the single NIC and decides their fate:
//   - VIP/hostIP → reverse NAT + write to TUN (bypass gateway return path)
//   - mesh (non-local) → mesh interceptor (mesh link)
//   - other → re-inject for local delivery (forwarder/hijacker receive via promiscuous mode)
func (e *Engine) writeLoop() {
	defer e.wg.Done()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-e.closeCh
		cancel()
	}()

	for {
		pkt := e.linkEP.ReadContext(ctx)
		if pkt == nil {
			return
		}

		buf := pkt.ToBuffer()
		data := buf.Flatten()

		if len(data) < 20 || (data[0]>>4) != 4 {
			pkt.DecRef()
			continue
		}

		dstIP := net.IP(data[16:20])

		if e.writePackets.Load() < 10 {
			util.LogDebug("tun writeLoop pkt#%d: %s -> %s (proto=%d len=%d)",
				e.writePackets.Load(),
				net.IP(data[12:16]), dstIP,
				data[9], len(data))
		}

		// Classify destination
		isVIP := false
		isHostIP := false
		if e.meshSubnet != nil {
			meshIP := e.meshSubnet.IP.To4()
			vip := net.IP{meshIP[0], meshIP[1], meshIP[2], meshIP[3] + 1}
			hostIP := net.IP{meshIP[0], meshIP[1], meshIP[2], meshIP[3] + 2}
			isVIP = dstIP.Equal(vip)
			isHostIP = dstIP.Equal(hostIP)
		}

		if isVIP || isHostIP {
			// Bypass gateway return path: write to TUN
			if isVIP && e.natTable != nil {
				hl := int(data[0]&0x0f) * 4
				if natPkt := e.natTable.TranslateInbound(data); natPkt != nil {
					nhl := int(natPkt[0]&0x0f) * 4
					util.LogDebug("tun writeLoop reverseNAT: %s:%d -> %s:%d (proto=%d)",
						net.IP(natPkt[12:16]), uint16(natPkt[nhl])<<8|uint16(natPkt[nhl+1]),
						net.IP(natPkt[16:20]), uint16(natPkt[nhl+2])<<8|uint16(natPkt[nhl+3]),
						natPkt[9])
					data = natPkt
				} else {
					util.LogDebug("tun writeLoop reverseNAT DROP: %s -> %s (proto=%d len=%d)",
						net.IP(data[12:16]), dstIP, data[9], len(data))
					pkt.DecRef()
					continue
				}
				_ = hl
			}

			if e.device != nil {
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
			}

		} else if e.meshInterceptor != nil && !e.isLocalMeshVIP(dstIP) {
			// Mesh interception: route packets destined for remote mesh nodes via mesh.
			// Also route packets FROM mesh VIPs to non-mesh destinations through the mesh
			// (e.g., TCP forwarder SYN-ACK responses to external clients via advertised routes).
			srcIP := net.IP(data[12:16])
			isMeshDst := e.isMeshIP(dstIP)
			isMeshSrc := e.isMeshIP(srcIP)
			if !isMeshDst && isMeshSrc {
				// Packet from mesh VIP to external IP: route through mesh so the
				// response reaches the original mesh peer's client.
				pktBuf := make([]byte, len(data))
				copy(pktBuf, data)
				select {
				case e.meshOutboundCh <- meshOutboundPacket{dstIP: dstIP, data: pktBuf}:
				default:
					util.LogWarn("[MESH-DIAG] meshOutboundCh full in writeLoop mesh-src (%d pending)", len(e.meshOutboundCh))
					pkt.DecRef()
				}
			} else {
				pktBuf := make([]byte, len(data))
				copy(pktBuf, data)
				select {
				case e.meshOutboundCh <- meshOutboundPacket{dstIP: dstIP, data: pktBuf}:
					// Queued for async mesh processing
				default:
					// Queue full — re-inject for local delivery as fallback
					util.LogWarn("[MESH-DIAG] meshOutboundCh full in writeLoop (%d pending), re-injecting", len(e.meshOutboundCh))
					newPkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
						Payload: buffer.MakeWithData(data),
					})
					e.linkEP.InjectInbound(ipv4.ProtocolNumber, newPkt)
					newPkt.DecRef()
				}
			}

		} else {
			// Re-inject for local delivery (forwarder/hijacker receive via promiscuous mode)
			select {
			case <-e.closeCh:
				pkt.DecRef()
				return
			default:
			}
			newPkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
				Payload: buffer.MakeWithData(data),
			})
			e.linkEP.InjectInbound(ipv4.ProtocolNumber, newPkt)
			newPkt.DecRef()
		}

		pkt.DecRef()
	}
}

// acceptTCP accepts TCP connections from netstack and proxies them.
func (e *Engine) acceptTCP() {
	defer e.wg.Done()

	fwd := tcp.NewForwarder(e.ns, 0, 1024, func(r *tcp.ForwarderRequest) {
		id := r.ID()
		util.LogDebug("[TCP-DEBUG] tcp forwarder called local=%s:%d remote=%s:%d",
			net.IP(id.LocalAddress.AsSlice()), id.LocalPort,
			net.IP(id.RemoteAddress.AsSlice()), id.RemotePort)
		var wq waiter.Queue
		util.LogDebug("[TCP-DEBUG] calling CreateEndpoint...")
		ep, err := r.CreateEndpoint(&wq)
		util.LogDebug("[TCP-DEBUG] CreateEndpoint returned, err=%v", err)
		if err != nil {
			util.LogWarn("[TCP-DEBUG] tcp CreateEndpoint fail: %v (local=%s:%d remote=%s:%d)",
				err, net.IP(id.LocalAddress.AsSlice()), id.LocalPort,
				net.IP(id.RemoteAddress.AsSlice()), id.RemotePort)
			r.Complete(true)
			return
		}
		util.LogDebug("[TCP-DEBUG] CreateEndpoint succeeded, calling handleConn async")
		r.Complete(false)

		conn := gonet.NewTCPConn(&wq, ep)
		dstIP := net.IP(id.LocalAddress.AsSlice())
		dstAddr := dstIP.String()
		dstPort := int(id.LocalPort)
		srcIP := net.IP(id.RemoteAddress.AsSlice())
		if e.natTable != nil {
			srcIP, _ = e.natTable.ResolveOriginalSrc(6, srcIP, id.RemotePort)
		}
		srcAddr := srcIP.String()
		inbound := ""
		var modeBMapping *config.Mapping
		// If destination matches a Mode B registration, look up the real client, inbound type, and mapping.
		// Use the remote port (srcPort from mesh peer) to avoid collisions when multiple clients
		// connect to the same destination.
		if e.modeBTable != nil {
			if clientAddr, modeBInbound, mapping := e.modeBTable.LookupByDst(6, dstIP, id.LocalPort, id.RemotePort); clientAddr != "" {
				srcAddr = clientAddr
				inbound = modeBInbound
				modeBMapping = mapping
			}
		}

		// Diagnostic: log connections involving mesh peers or advertised routes
		isMeshIP := e.isMeshIP(dstIP)
		isMeshSrc := e.isMeshIP(srcIP)
		if isMeshSrc && !isMeshIP {
			if modeBMapping != nil {
				util.LogInfo("[TCP-DIAG] advertised route conn: src=%s dst=%s:%d modeB=%s inbound=%s",
					srcAddr, dstAddr, dstPort, modeBMapping.Name, inbound)
			} else {
				util.LogInfo("[TCP-DIAG] gateway fwd conn: src=%s dst=%s:%d (no modeB)",
					srcAddr, dstAddr, dstPort)
			}
		} else if !isMeshIP && modeBMapping != nil {
			util.LogInfo("[TCP-DIAG] advertised route conn: src=%s dst=%s:%d modeB=%s inbound=%s",
				srcAddr, dstAddr, dstPort, modeBMapping.Name, inbound)
		}

		go func() {
			defer ep.Close()
			defer conn.Close()
			e.handleConn(conn, srcAddr, dstAddr, dstPort, inbound, modeBMapping)
		}()
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
			dstIP := net.IP(id.LocalAddress.AsSlice())
			dstAddr := dstIP.String()
			dstPort := int(id.LocalPort)
			srcIP := net.IP(id.RemoteAddress.AsSlice())
			if e.natTable != nil {
				srcIP, _ = e.natTable.ResolveOriginalSrc(17, srcIP, id.RemotePort)
			}
			srcAddr := srcIP.String()
			inbound := ""
			var modeBMapping *config.Mapping
			// If destination matches a Mode B registration, look up the real client, inbound type, and mapping.
			// Use the remote port (srcPort from mesh peer) to avoid collisions.
			if e.modeBTable != nil {
				if clientAddr, modeBInbound, mapping := e.modeBTable.LookupByDst(17, dstIP, id.LocalPort, id.RemotePort); clientAddr != "" {
					srcAddr = clientAddr
					inbound = modeBInbound
					modeBMapping = mapping
				}
			}

			conn := gonet.NewUDPConn(&wq, ep)
			defer conn.Close()

			e.handleUDP(conn, srcAddr, dstAddr, dstPort, inbound, modeBMapping)
		}()
	})

	e.ns.SetTransportProtocolHandler(udp.ProtocolNumber, fwd.HandlePacket)

	<-e.closeCh
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

	req := config.NewConnectRequest(matchAddr, dstPort)
	var proxy *config.Proxy
	var matchResult *config.MatchResult
	if e.ruleConf != nil {
		req = e.ruleConf.Resolving(req)
		matchMapping := TUNMapping
		if modeBMapping != nil {
			matchMapping = modeBMapping
		}
		proxy, matchResult = e.ruleConf.Match(req, matchMapping)
	}

	resolvedAddr := req.DstAddr
	resolvedPort := req.DstPort

	if proxy != nil && strings.ToUpper(proxy.Type) == config.ProxyREJECT {
		util.LogDebug("[%s] [%s] udp %s:%d -> REJECTED", inbound, connID, resolvedAddr, resolvedPort)
		connlog.Log(inbound, "UDP", srcAddr, matchAddr, resolvedAddr, resolvedPort, matchResult, "reject", nil)
		return
	}

	var targetConn net.PacketConn
	var err error
	var dialIP net.IP

	if proxy != nil && strings.ToUpper(proxy.Type) != config.ProxyDIRECT {
		targetConn, err = dialer.ChainUDPDial(proxy)
		if err != nil {
			util.LogWarn("[%s] [%s] udp dial %s:%d via %s fail: %v", inbound, connID, resolvedAddr, resolvedPort, proxy.Name, err)
			connlog.Log(inbound, "UDP", srcAddr, matchAddr, resolvedAddr, resolvedPort, matchResult, "fail", err)
			return
		}
		dialIP = net.ParseIP(resolvedAddr)
	} else {
		// Direct dial: resolve real IP now if we have a domain.
		dialIP = net.ParseIP(resolvedAddr)
		if localNodeDomain {
			// Local mesh nodeID domain: connect to localhost
			dialIP = net.ParseIP("127.0.0.1")
			util.LogDebug("[%s] [%s] udp local mesh nodeID domain: %s -> %s", inbound, connID, domain, dialIP)
		} else if domain != "" {
			// Resolve the real IP for DIRECT connections.
			ips, err := e.resolveForDirect(domain)
			if err != nil || len(ips) == 0 {
				util.LogWarn("[%s] [%s] udp resolve %s fail: %v", inbound, connID, domain, err)
				connlog.Log(inbound, "UDP", srcAddr, matchAddr, domain, resolvedPort, &config.MatchResult{ProxyName: "DIRECT"}, "fail", err)
				return
			}
			// Prefer IPv4
			for _, ip := range ips {
				if ip4 := ip.To4(); ip4 != nil {
					dialIP = ip4
					break
				}
			}
			util.LogDebug("[%s] [%s] udp resolved %s -> %s for DIRECT", inbound, connID, domain, dialIP)
		}
		targetConn, err = dialer.ListenPacketBoundTo("udp", "", dialIP)
		if err != nil {
			util.LogWarn("[%s] [%s] udp direct dial %s:%d fail: %v", inbound, connID, resolvedAddr, resolvedPort, err)
			connlog.Log(inbound, "UDP", srcAddr, matchAddr, dialIP.String(), resolvedPort, &config.MatchResult{ProxyName: "DIRECT"}, "fail", err)
			return
		}
	}
	defer targetConn.Close()

	// Use the resolved IP for the destination address.
	dstUDPAddr := &net.UDPAddr{IP: dialIP, Port: resolvedPort}
	util.LogDebug("[%s] [%s] udp %s:%d -> %s", inbound, connID, resolvedAddr, resolvedPort, proxyDesc(proxy))
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
	connlog.Log(inbound, "UDP", srcAddr, matchAddr, resolvedAddr, resolvedPort, matchResult, "ok", nil)
	connlog.TrackActive(connID, inbound, "UDP", srcAddr, matchAddr, resolvedAddr, resolvedPort, matchResult)
	defer connlog.RemoveActive(connID)

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

	req := config.NewConnectRequest(matchAddr, dstPort)
	var proxy *config.Proxy
	var matchResult *config.MatchResult
	if e.ruleConf != nil {
		req = e.ruleConf.Resolving(req)
		matchMapping := TUNMapping
		if modeBMapping != nil {
			matchMapping = modeBMapping
		}
		proxy, matchResult = e.ruleConf.Match(req, matchMapping)
		if proxy != nil {
			util.LogDebug("[TCP-DEBUG] rule match: %s:%d -> proxy=%s type=%s", matchAddr, dstPort, proxy.Name, proxy.Type)
		} else {
			util.LogDebug("[TCP-DEBUG] rule match: %s:%d -> DIRECT (no proxy matched)", matchAddr, dstPort)
		}
	}

	// Use the (possibly redirected) destination from Resolving
	resolvedAddr := req.DstAddr
	resolvedPort := req.DstPort

	var targetConn net.Conn
	var err error

	if proxy != nil && strings.ToUpper(proxy.Type) == config.ProxyREJECT {
		util.LogDebug("[%s] [%s] %s:%d -> REJECTED", inbound, connID, resolvedAddr, resolvedPort)
		connlog.Log(inbound, "TCP", srcAddr, matchAddr, resolvedAddr, resolvedPort, matchResult, "reject", nil)
		return
	}

	if localNodeDomain {
		// Direct admin handler: bypass OS network stack entirely
		if e.adminHandler != nil {
			util.LogDebug("[TCP-DEBUG] [%s] localNodeDomain=true, serving via admin handler directly (bypassing OS network stack)", connID)
			connlog.Log(inbound, "TCP", srcAddr, matchAddr, "localhost", resolvedPort, matchResult, "ok", nil)
			connlog.TrackActive(connID, inbound, "TCP", srcAddr, matchAddr, "localhost", resolvedPort, matchResult)
			defer connlog.RemoveActive(connID)
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
			connlog.Log(inbound, "TCP", srcAddr, matchAddr, dialAddr, resolvedPort, matchResult, "fail", err)
			return
		}
	} else if proxy != nil && strings.ToUpper(proxy.Type) != config.ProxyDIRECT {
		util.LogDebug("[TCP-DEBUG] [%s] dialing via proxy %s: %s:%d", connID, proxy.Name, resolvedAddr, resolvedPort)
		targetConn, err = dialer.ChainDialWithID(proxy, resolvedAddr, resolvedPort, connID)
		if err != nil {
			util.LogWarn("[%s] [%s] dial %s:%d via %s fail: %v", inbound, connID, resolvedAddr, resolvedPort, proxy.Name, err)
			connlog.Log(inbound, "TCP", srcAddr, matchAddr, resolvedAddr, resolvedPort, matchResult, "fail", err)
			return
		}
		util.LogDebug("[TCP-DEBUG] [%s] proxy dial success", connID)
	} else {
		// Direct dial: resolve real IP now if we have a domain.
		dialAddr := resolvedAddr
		if domain != "" {
			// Resolve the real IP for DIRECT connections.
			util.LogDebug("[TCP-DEBUG] [%s] resolving %s for DIRECT dial", connID, domain)
			ips, err := e.resolveForDirect(domain)
			if err != nil || len(ips) == 0 {
				util.LogWarn("[%s] [%s] resolve %s fail: %v", inbound, connID, domain, err)
				connlog.Log(inbound, "TCP", srcAddr, matchAddr, domain, resolvedPort, &config.MatchResult{ProxyName: "DIRECT"}, "fail", err)
				return
			}
			// Prefer IPv4
			for _, ip := range ips {
				if ip4 := ip.To4(); ip4 != nil {
					dialAddr = ip4.String()
					break
				}
			}
			util.LogDebug("[TCP-DEBUG] [%s] resolved %s -> %s", connID, domain, dialAddr)
		} else {
			util.LogDebug("[TCP-DEBUG] [%s] direct dial with no domain, addr=%s", connID, dialAddr)
		}
		util.LogDebug("[TCP-DEBUG] [%s] dialing tcp %s:%d", connID, dialAddr, resolvedPort)
		targetConn, err = dialer.DialRouteAware("tcp", net.JoinHostPort(dialAddr, fmt.Sprintf("%d", resolvedPort)))
		if err != nil {
			util.LogWarn("[%s] [%s] direct dial %s:%d fail: %v", inbound, connID, dialAddr, resolvedPort, err)
			connlog.Log(inbound, "TCP", srcAddr, matchAddr, dialAddr, resolvedPort, &config.MatchResult{ProxyName: "DIRECT"}, "fail", err)
			return
		}
		util.LogDebug("[TCP-DEBUG] [%s] direct dial success, starting relay", connID)
	}
	defer targetConn.Close()

	util.LogDebug("[TCP-DEBUG] [%s] relay started: %s:%d -> %s", connID, resolvedAddr, resolvedPort, proxyDesc(proxy))
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
	connlog.Log(inbound, "TCP", srcAddr, matchAddr, resolvedAddr, resolvedPort, matchResult, "ok", nil)
	connlog.TrackActive(connID, inbound, "TCP", srcAddr, matchAddr, resolvedAddr, resolvedPort, matchResult)
	defer connlog.RemoveActive(connID)
	relayWithIdleTimeout(conn, targetConn, 90*time.Second)
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

	// Use netstack UDP path to the DNS hijacker, which handles cross-node
	// forwarding internally (local domains allocate Fake-IP, remote domains
	// are forwarded to the remote node's DNS hijacker via mesh).
	remoteAddr := tcpip.FullAddress{NIC: 1, Addr: e.dnsAddr, Port: 53}
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
