package mesh

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"phaethon/config"
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

// ForwarderCallbacks provides the connection handling callbacks for TCP and UDP
// forwarders. These are set by the TUN engine so that the netstack can delegate
// connection handling without importing the tun package.
type ForwarderCallbacks struct {
	HandleTCPConn func(conn net.Conn, srcAddr, dstAddr string, dstPort int, inbound string, modeBMapping *config.Mapping)
	HandleUDPConn func(conn net.Conn, srcAddr, dstAddr string, dstPort int, inbound string, modeBMapping *config.Mapping)
	// ResolveOriginalSrc resolves the original source IP from the NAT table.
	// proto is 6 for TCP, 17 for UDP.
	ResolveOriginalSrc func(proto int, srcIP net.IP, srcPort uint16) (net.IP, bool)
	// LookupModeB looks up a Mode B registration by destination.
	// proto is 6 for TCP, 17 for UDP.
	LookupModeB func(proto int, dstIP net.IP, dstPort, srcPort uint16) (clientAddr, inbound string, mapping *config.Mapping)
	// IsMeshIP checks if an IP belongs to the mesh network.
	IsMeshIP func(ip net.IP) bool
	// StatsNotify is called when packet counters change.
	StatsNotify func()
}

// Netstack manages the gVisor netstack, link endpoint, and h_tunnel endpoints.
// It is created once and shared with the TUN engine for device I/O.
type Netstack struct {
	mu sync.Mutex

	ns     *stack.Stack
	linkEP *channel.Endpoint

	// Addresses derived from mesh subnet
	addr    tcpip.Address // hostIP (.2) - TUN adapter OS side
	dnsAddr tcpip.Address // GIP (.3) - netstack internal, DNS, proxy socket source
	addrSet bool

	// Running state
	running bool
	closeCh chan struct{}
	wg      sync.WaitGroup

	// h_tunnel netstack endpoints (one per h_tunnel proxy, uplink via HTTP)
	htunnelMu        sync.RWMutex
	htunnelEndpoints map[string]*HTunnelEndpoint
	htunnelNextNICID atomic.Uint64

	// Forwarder callbacks (set by TUN engine)
	callbacks *ForwarderCallbacks

	// WriteLoopDevice is the TUN device for writeLoop to write packets to.
	// Set by the TUN engine.
	WriteLoopDevice interface {
		Write(data []byte) (int, error)
	}

	// WriteLoopCloseCh returns the close channel for writeLoop cancellation.
	// Set by the TUN engine.
	WriteLoopCloseCh func() <-chan struct{}

	// Mesh subnet and NAT for writeLoop routing decisions
	meshSubnet *net.IPNet
	natTable   *NATTable

	// Mesh interception for writeLoop
	meshInterceptor func(dstIP net.IP, data []byte) bool
	isLocalMeshVIP  func(ip net.IP) bool
	isMeshIPFunc    func(ip net.IP) bool

	// MeshOutboundFunc is called by writeLoop for packets destined for mesh.
	// Returns true if the packet was queued for async mesh processing.
	// Set by the TUN engine.
	MeshOutboundFunc func(dstIP net.IP, data []byte) bool

	// MeshOutboundFullFunc is called when the mesh outbound queue is full.
	// Set by the TUN engine.
	MeshOutboundFullFunc func(dstIP net.IP, data []byte)

	// Packet counters for diagnostics
	ReadPackets  atomic.Uint64
	WritePackets atomic.Uint64
}

// NewNetstack creates a new Netstack instance.
func NewNetstack() *Netstack {
	return &Netstack{
		htunnelEndpoints: make(map[string]*HTunnelEndpoint),
	}
}

// Stack returns the underlying gVisor stack.
func (n *Netstack) Stack() *stack.Stack {
	return n.ns
}

// LinkEP returns the channel link endpoint.
func (n *Netstack) LinkEP() *channel.Endpoint {
	return n.linkEP
}

// Addr returns the host IP address (TUN adapter OS side, .2).
func (n *Netstack) Addr() tcpip.Address {
	return n.addr
}

// DNSAddr returns the GIP address (netstack DNS, .3).
func (n *Netstack) DNSAddr() tcpip.Address {
	return n.dnsAddr
}

// CloseCh returns the close channel for stack goroutine cancellation.
func (n *Netstack) CloseCh() <-chan struct{} {
	return n.closeCh
}

// WaitGroup returns the wait group for stack goroutines.
func (n *Netstack) WaitGroup() *sync.WaitGroup {
	return &n.wg
}

// IsRunning reports whether the netstack is running.
func (n *Netstack) IsRunning() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.running
}

// SetMeshSubnet sets the mesh subnet for writeLoop routing decisions.
func (n *Netstack) SetMeshSubnet(subnet *net.IPNet) {
	n.meshSubnet = subnet
}

// SetNATTable sets the NAT table for writeLoop reverse NAT.
func (n *Netstack) SetNATTable(nat *NATTable) {
	n.natTable = nat
}

// SetMeshInterceptor sets the mesh interception callback for writeLoop.
func (n *Netstack) SetMeshInterceptor(handler func(dstIP net.IP, data []byte) bool) {
	n.meshInterceptor = handler
}

// SetLocalMeshVIPFunc sets the function to check if an IP is a local mesh VIP.
func (n *Netstack) SetLocalMeshVIPFunc(f func(ip net.IP) bool) {
	n.isLocalMeshVIP = f
}

// SetIsMeshIPFunc sets the function to check if an IP belongs to the mesh network.
func (n *Netstack) SetIsMeshIPFunc(f func(ip net.IP) bool) {
	n.isMeshIPFunc = f
}

// SetCallbacks sets the forwarder callbacks.
func (n *Netstack) SetCallbacks(cb *ForwarderCallbacks) {
	n.callbacks = cb
}

// ConfigureMeshAddresses computes and stores the mesh-derived addresses.
// VIP (.1) = mesh routing + NAT source, hostIP (.2) = TUN adapter, GIP (.3) = netstack/DNS.
func (n *Netstack) ConfigureMeshAddresses(subnet *net.IPNet) error {
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

	n.addr = tcpip.AddrFrom4Slice(hostIP)
	n.dnsAddr = tcpip.AddrFrom4Slice(gip)
	n.addrSet = true

	util.LogDebug("netstack: mesh addresses: hostIP=%s GIP=%s", hostIP, gip)
	return nil
}

// AddrSet reports whether mesh addresses have been configured.
func (n *Netstack) AddrSet() bool {
	return n.addrSet
}

// Start initializes the gVisor netstack and starts all stack goroutines.
func (n *Netstack) Start() error {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.running {
		return fmt.Errorf("netstack already running")
	}

	if !n.addrSet {
		return fmt.Errorf("netstack: addresses not configured (ConfigureMeshAddresses must be called before Start)")
	}

	// Initialize the gVisor stack
	if err := n.initStack(); err != nil {
		return fmt.Errorf("netstack: init: %w", err)
	}

	// Start stack-level goroutines
	n.running = true
	n.closeCh = make(chan struct{})

	n.wg.Add(3)
	go n.acceptTCP()
	go n.acceptUDP()
	go n.writeLoop()

	// Diagnostic goroutine
	n.wg.Add(1)
	go n.logPacketCounts()

	util.LogDebug("netstack: started")
	return nil
}

// Stop stops the gVisor netstack and waits for all goroutines to finish.
func (n *Netstack) Stop() error {
	n.mu.Lock()
	if !n.running {
		n.mu.Unlock()
		return nil
	}
	n.running = false
	close(n.closeCh)
	n.mu.Unlock()

	// Wait for stack goroutines to finish
	n.wg.Wait()

	if n.ns != nil {
		n.ns.Close()
	}

	util.LogDebug("netstack: stopped")
	return nil
}

// initStack creates the gvisor netstack with a single NIC.
// All traffic (TUN, DNS hijacker, TCP forwarder, Mode B sockets) shares one NIC.
// writeLoop handles all routing decisions: VIP/hostIP -> TUN, mesh -> mesh link, other -> re-inject.
func (n *Netstack) initStack() error {
	linkEP := channel.New(8192, 1500, "")
	n.linkEP = linkEP

	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	n.ns = s

	if err := s.CreateNIC(1, linkEP); err != nil {
		return fmt.Errorf("create nic: %v", err)
	}

	ap := tcpip.AddressWithPrefix{Address: n.dnsAddr, PrefixLen: 32}
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

// InjectMeshPacket injects a raw IP packet into the netstack as if received from the TUN device.
// Used by the mesh module to deliver received overlay packets to the local TCP/IP stack.
func (n *Netstack) InjectMeshPacket(data []byte) error {
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
		util.LogDebug("netstack: InjectMeshPacket %s -> %s proto=%d len=%d", srcIP, dstIP, proto, len(data))
		if len(data) >= 20 && data[9] == 6 {
			logTCPPacket("[TCP-DEBUG] InjectMeshPacket:", data)
			headerLen := int(data[0]&0x0f) * 4
			if len(data) >= headerLen+14 {
				flags := data[headerLen+13]
				isSYN := (flags&0x02) != 0 && (flags&0x10) == 0
				if isSYN {
					dstPort := uint16(data[headerLen+2])<<8 | uint16(data[headerLen+3])
					util.LogInfo("[TCP-DIAG] InjectMeshPacket SYN: src=%s dst=%s:%d len=%d",
						srcIP, dstIP, dstPort, len(data))
				}
			}
		}
	}

	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(data),
	})
	n.linkEP.InjectInbound(proto, pkt)
	pkt.DecRef()
	return nil
}

// InjectInbound injects a raw IP packet into the netstack via the link endpoint.
// Used by the TUN engine's meshOutboundLoop for packet re-injection.
func (n *Netstack) InjectInbound(proto tcpip.NetworkProtocolNumber, data []byte) {
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(data),
	})
	n.linkEP.InjectInbound(proto, pkt)
	pkt.DecRef()
}

// AddMeshVIP registers a mesh virtual IP with the gVisor netstack so it responds
// to packets (e.g., ICMP) destined for that IP.
func (n *Netstack) AddMeshVIP(vip net.IP) error {
	if n.ns == nil {
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
	if err := n.ns.AddProtocolAddress(1, protoAddr, stack.AddressProperties{}); err != nil {
		return fmt.Errorf("add mesh VIP %s: %v", vip, err)
	}
	util.LogDebug("netstack: registered mesh VIP %s", vip)
	return nil
}

// NetDial dials a connection through the netstack.
func (n *Netstack) NetDial(network, addr string) (net.Conn, error) {
	util.LogDebug("netstack: NetDial called with network=%s addr=%s", network, addr)
	n.mu.Lock()
	running := n.running
	ns := n.ns
	n.mu.Unlock()

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
		conn, err := n.dialTCP(ctx, ns, remoteAddr)
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

// dialTCP creates a TCP connection through the netstack.
func (n *Netstack) dialTCP(ctx context.Context, s *stack.Stack, remoteAddr tcpip.FullAddress) (net.Conn, error) {
	var wq waiter.Queue
	ep, tcpErr := s.NewEndpoint(tcp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
	if tcpErr != nil {
		return nil, fmt.Errorf("create endpoint: %s", tcpErr)
	}

	// Bind to GIP (dnsAddr) with port 0 to allocate a source port
	localAddr := tcpip.FullAddress{
		Addr: n.dnsAddr,
		Port: 0,
	}
	if tcpErr := ep.Bind(localAddr); tcpErr != nil {
		ep.Close()
		return nil, fmt.Errorf("bind: %s", tcpErr)
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

// NetDialWithPreConnect dials through the netstack with a pre-connect callback.
// The callback is called after bind (port allocated) but before connect (SYN sent),
// allowing ModeBTable registration before the forwarder is triggered.
func (n *Netstack) NetDialWithPreConnect(network, addr string, preConnect func(dstAddr tcpip.Address, dstPort, srcPort uint16)) (net.Conn, error) {
	n.mu.Lock()
	running := n.running
	ns := n.ns
	n.mu.Unlock()

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

	var wq waiter.Queue
	ep, tcpErr := ns.NewEndpoint(tcp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
	if tcpErr != nil {
		return nil, fmt.Errorf("create endpoint: %s", tcpErr)
	}

	// Bind to GIP (dnsAddr) with port 0 to allocate a source port
	localAddr := tcpip.FullAddress{
		Addr: n.dnsAddr,
		Port: 0,
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
	if preConnect != nil {
		preConnect(remoteAddr.Addr, remoteAddr.Port, srcPort)
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
func (n *Netstack) NetDialWithModeB(network, addr string, clientAddr string, inbound string, mapping *config.Mapping, modeBTable *ModeBTable) (net.Conn, error) {
	util.LogDebug("netstack: NetDialWithModeB called with network=%s addr=%s client=%s", network, addr, clientAddr)
	n.mu.Lock()
	running := n.running
	ns := n.ns
	n.mu.Unlock()

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
		Addr: n.dnsAddr,
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

// ResolveDomain resolves a domain name by sending a DNS query through the
// netstack UDP socket to the local hijacker (dnsAddr:53).
func (n *Netstack) ResolveDomain(domain string) (net.IP, error) {
	util.LogDebug("netstack: ResolveDomain called for domain=%s", domain)
	n.mu.Lock()
	ns := n.ns
	dnsAddr := n.dnsAddr
	n.mu.Unlock()
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

	// Register waiter BEFORE write -- loopback delivery is synchronous.
	waitEntry, ch := waiter.NewChannelEntry(waiter.EventIn)
	wq.EventRegister(&waitEntry)
	defer wq.EventUnregister(&waitEntry)

	txID := uint16(time.Now().UnixNano())
	query := BuildDNSQuery(domain, txID)
	if _, err := ep.Write(&SlicePayload{Data: query}, tcpip.WriteOptions{}); err != nil {
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
	fakeIP, _ := ParseDNSResponseIP(buf.Bytes())
	if fakeIP == nil {
		return nil, fmt.Errorf("resolve %s: bad response", domain)
	}
	return fakeIP, nil
}

// QueryInternalDNS sends a raw DNS query to the TUN DNS hijacker and returns
// the raw response bytes.
func (n *Netstack) QueryInternalDNS(query []byte) ([]byte, error) {
	if !n.IsRunning() || n.ns == nil {
		return nil, fmt.Errorf("netstack not running")
	}

	remoteAddr := tcpip.FullAddress{NIC: 1, Addr: n.dnsAddr, Port: 53}
	conn, err := gonet.DialUDP(n.ns, nil, &remoteAddr, ipv4.ProtocolNumber)
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
	readN, err := conn.Read(resp)
	if err != nil {
		return nil, fmt.Errorf("read fail: %v", err)
	}
	return resp[:readN], nil
}

// AddHTunnelEndpoint creates a link.Endpoint for an h_tunnel proxy and registers
// it with the netstack.
func (n *Netstack) AddHTunnelEndpoint(proxyName string, peer PeerSender) error {
	n.htunnelMu.Lock()
	defer n.htunnelMu.Unlock()

	if n.htunnelEndpoints == nil {
		n.htunnelEndpoints = make(map[string]*HTunnelEndpoint)
	}
	if _, exists := n.htunnelEndpoints[proxyName]; exists {
		return nil
	}

	if n.ns == nil {
		return fmt.Errorf("netstack not initialized")
	}

	nicID := tcpip.NICID(100 + n.htunnelNextNICID.Add(1))
	ep := NewHTunnelEndpoint(nicID, peer)

	if err := n.ns.CreateNIC(nicID, ep); err != nil {
		return fmt.Errorf("create h_tunnel NIC: %v", err)
	}

	n.ns.SetPromiscuousMode(nicID, true)
	n.ns.SetSpoofing(nicID, true)

	n.htunnelEndpoints[proxyName] = ep
	util.LogInfo("[HTUNNEL-EP] added endpoint for proxy %s (NIC=%d)", proxyName, nicID)
	return nil
}

// RemoveHTunnelEndpoint removes and closes the h_tunnel endpoint for a proxy.
func (n *Netstack) RemoveHTunnelEndpoint(proxyName string) {
	n.htunnelMu.Lock()
	defer n.htunnelMu.Unlock()

	ep, ok := n.htunnelEndpoints[proxyName]
	if !ok {
		return
	}

	ep.Close()
	if n.ns != nil {
		n.ns.RemoveNIC(ep.NicID())
	}
	delete(n.htunnelEndpoints, proxyName)
	util.LogInfo("[HTUNNEL-EP] removed endpoint for proxy %s (NIC=%d)", proxyName, ep.NicID())
}

// HTunnelEndpoint returns the h_tunnel endpoint for a proxy, or nil if not found.
func (n *Netstack) HTunnelEndpoint(proxyName string) *HTunnelEndpoint {
	n.htunnelMu.RLock()
	defer n.htunnelMu.RUnlock()
	return n.htunnelEndpoints[proxyName]
}

// logPacketCounts periodically logs packet counters for diagnostics.
func (n *Netstack) logPacketCounts() {
	defer n.wg.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-n.closeCh:
			return
		case <-ticker.C:
			util.LogDebug("netstack counters: read=%d write=%d", n.ReadPackets.Load(), n.WritePackets.Load())
		}
	}
}

// acceptTCP accepts TCP connections from netstack and delegates to the callback.
func (n *Netstack) acceptTCP() {
	defer n.wg.Done()

	fwd := tcp.NewForwarder(n.ns, 0, 1024, func(r *tcp.ForwarderRequest) {
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

		// Set aggressive TCP keepalive on gVisor endpoint to detect dead clients faster
		idle := tcpip.KeepaliveIdleOption(30 * time.Second)
		if err := ep.SetSockOpt(&idle); err != nil {
			util.LogDebug("[TCP-DEBUG] failed to set keepalive idle: %v", err)
		}
		interval := tcpip.KeepaliveIntervalOption(10 * time.Second)
		if err := ep.SetSockOpt(&interval); err != nil {
			util.LogDebug("[TCP-DEBUG] failed to set keepalive interval: %v", err)
		}
		count := tcpip.SockOptInt(3)
		if err := ep.SetSockOptInt(tcpip.KeepaliveCountOption, int(count)); err != nil {
			util.LogDebug("[TCP-DEBUG] failed to set keepalive count: %v", err)
		}

		conn := gonet.NewTCPConn(&wq, ep)
		dstIP := net.IP(id.LocalAddress.AsSlice())
		dstAddr := dstIP.String()
		dstPort := int(id.LocalPort)
		srcIP := net.IP(id.RemoteAddress.AsSlice())
		if n.callbacks != nil && n.callbacks.ResolveOriginalSrc != nil {
			srcIP, _ = n.callbacks.ResolveOriginalSrc(6, srcIP, id.RemotePort)
		}
		srcAddr := srcIP.String()
		inbound := ""
		var modeBMapping *config.Mapping
		if n.callbacks != nil && n.callbacks.LookupModeB != nil {
			if clientAddr, modeBInbound, mapping := n.callbacks.LookupModeB(6, dstIP, id.LocalPort, id.RemotePort); clientAddr != "" {
				srcAddr = clientAddr
				inbound = modeBInbound
				modeBMapping = mapping
			}
		}

		// Diagnostic: log connections involving mesh peers or advertised routes
		if n.callbacks != nil && n.callbacks.IsMeshIP != nil {
			isMeshIP := n.callbacks.IsMeshIP(dstIP)
			isMeshSrc := n.callbacks.IsMeshIP(srcIP)
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
		}

		if n.callbacks != nil && n.callbacks.HandleTCPConn != nil {
			go func() {
				defer ep.Close()
				defer conn.Close()
				n.callbacks.HandleTCPConn(conn, srcAddr, dstAddr, dstPort, inbound, modeBMapping)
			}()
		} else {
			ep.Close()
			conn.Close()
		}
	})

	n.ns.SetTransportProtocolHandler(tcp.ProtocolNumber, fwd.HandlePacket)

	<-n.closeCh
}

// acceptUDP accepts UDP datagrams from netstack and delegates to the callback.
func (n *Netstack) acceptUDP() {
	defer n.wg.Done()

	fwd := udp.NewForwarder(n.ns, func(r *udp.ForwarderRequest) {
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
			if n.callbacks != nil && n.callbacks.ResolveOriginalSrc != nil {
				srcIP, _ = n.callbacks.ResolveOriginalSrc(17, srcIP, id.RemotePort)
			}
			srcAddr := srcIP.String()
			inbound := ""
			var modeBMapping *config.Mapping
			if n.callbacks != nil && n.callbacks.LookupModeB != nil {
				if clientAddr, modeBInbound, mapping := n.callbacks.LookupModeB(17, dstIP, id.LocalPort, id.RemotePort); clientAddr != "" {
					srcAddr = clientAddr
					inbound = modeBInbound
					modeBMapping = mapping
				}
			}

			conn := gonet.NewUDPConn(&wq, ep)
			defer conn.Close()

			if n.callbacks != nil && n.callbacks.HandleUDPConn != nil {
				n.callbacks.HandleUDPConn(conn, srcAddr, dstAddr, dstPort, inbound, modeBMapping)
			}
		}()
	})

	n.ns.SetTransportProtocolHandler(udp.ProtocolNumber, fwd.HandlePacket)

	<-n.closeCh
}

// writeLoop reads outbound packets from the single NIC and decides their fate:
//   - VIP/hostIP -> reverse NAT + write to TUN (bypass gateway return path)
//   - mesh (non-local) -> mesh interceptor (mesh link)
//   - other -> re-inject for local delivery (forwarder/hijacker receive via promiscuous mode)
func (n *Netstack) writeLoop() {
	defer n.wg.Done()

	closeCh := n.WriteLoopCloseCh()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-closeCh:
			cancel()
		case <-n.closeCh:
			cancel()
		}
	}()

	for {
		pkt := n.linkEP.ReadContext(ctx)
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

		if n.WritePackets.Load() < 10 {
			util.LogDebug("netstack writeLoop pkt#%d: %s -> %s (proto=%d len=%d)",
				n.WritePackets.Load(),
				net.IP(data[12:16]), dstIP,
				data[9], len(data))
		}

		// Classify destination
		isVIP := false
		isHostIP := false
		if n.meshSubnet != nil {
			meshIP := n.meshSubnet.IP.To4()
			vip := net.IP{meshIP[0], meshIP[1], meshIP[2], meshIP[3] + 1}
			hostIP := net.IP{meshIP[0], meshIP[1], meshIP[2], meshIP[3] + 2}
			isVIP = dstIP.Equal(vip)
			isHostIP = dstIP.Equal(hostIP)
		}

		if isVIP || isHostIP {
			// Bypass gateway return path: write to TUN
			if isVIP && n.natTable != nil {
				hl := int(data[0]&0x0f) * 4
				if natPkt := n.natTable.TranslateInbound(data); natPkt != nil {
					nhl := int(natPkt[0]&0x0f) * 4
					util.LogDebug("netstack writeLoop reverseNAT: %s:%d -> %s:%d (proto=%d)",
						net.IP(natPkt[12:16]), uint16(natPkt[nhl])<<8|uint16(natPkt[nhl+1]),
						net.IP(natPkt[16:20]), uint16(natPkt[nhl+2])<<8|uint16(natPkt[nhl+3]),
						natPkt[9])
					data = natPkt
				} else {
					util.LogDebug("netstack writeLoop reverseNAT DROP: %s -> %s (proto=%d len=%d)",
						net.IP(data[12:16]), dstIP, data[9], len(data))
					pkt.DecRef()
					continue
				}
				_ = hl
			}

			if n.WriteLoopDevice != nil {
				if _, err := n.WriteLoopDevice.Write(data); err != nil {
					select {
					case <-n.closeCh:
						pkt.DecRef()
						return
					default:
						util.LogWarn("netstack: write error: %v", err)
					}
				} else {
					n.WritePackets.Add(1)
					if n.callbacks != nil && n.callbacks.StatsNotify != nil {
						n.callbacks.StatsNotify()
					}
				}
			}

		} else if n.meshInterceptor != nil && (n.isLocalMeshVIP == nil || !n.isLocalMeshVIP(dstIP)) {
			// Mesh interception: route packets destined for remote mesh nodes via mesh.
			// Also route packets FROM mesh VIPs to non-mesh destinations through the mesh
			// (e.g., TCP forwarder SYN-ACK responses to external clients via advertised routes).
			srcIP := net.IP(data[12:16])
			isMeshDst := n.isMeshIPFunc != nil && n.isMeshIPFunc(dstIP)
			isMeshSrc := n.isMeshIPFunc != nil && n.isMeshIPFunc(srcIP)
			if !isMeshDst && isMeshSrc {
				// Packet from mesh VIP to external IP: route through mesh so the
				// response reaches the original mesh peer's client.
				pktBuf := make([]byte, len(data))
				copy(pktBuf, data)
				if n.MeshOutboundFunc != nil {
					if !n.MeshOutboundFunc(dstIP, pktBuf) {
						util.LogWarn("[MESH-DIAG] meshOutboundCh full in writeLoop mesh-src, dropping")
					}
				}
			} else {
				pktBuf := make([]byte, len(data))
				copy(pktBuf, data)
				if n.MeshOutboundFunc != nil {
					if !n.MeshOutboundFunc(dstIP, pktBuf) {
						// Queue full -- re-inject for local delivery as fallback
						util.LogWarn("[MESH-DIAG] meshOutboundCh full in writeLoop (%d pending), re-injecting", 0)
						if n.MeshOutboundFullFunc != nil {
							n.MeshOutboundFullFunc(dstIP, pktBuf)
						} else {
							newPkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
								Payload: buffer.MakeWithData(pktBuf),
							})
							n.linkEP.InjectInbound(ipv4.ProtocolNumber, newPkt)
							newPkt.DecRef()
						}
					}
				}
			}

		} else {
			// Re-inject for local delivery (forwarder/hijacker receive via promiscuous mode)
			select {
			case <-n.closeCh:
				pkt.DecRef()
				return
			default:
			}
			newPkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
				Payload: buffer.MakeWithData(data),
			})
			n.linkEP.InjectInbound(ipv4.ProtocolNumber, newPkt)
			newPkt.DecRef()
		}

		pkt.DecRef()
	}
}

// fullToTCPAddr converts a tcpip.FullAddress to a net.TCPAddr
func fullToTCPAddr(addr tcpip.FullAddress) *net.TCPAddr {
	return &net.TCPAddr{
		IP:   net.IP(addr.Addr.AsSlice()),
		Port: int(addr.Port),
	}
}

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
