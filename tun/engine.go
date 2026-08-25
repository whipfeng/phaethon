package tun

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"syscall"
	"time"

	"phaethon/config"
	"phaethon/dialer"
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

// Engine manages the TUN device, netstack, and traffic interception.
type Engine struct {
	ruleConf  *config.RuleConfiguration
	device    Device
	linkEP    *channel.Endpoint
	ns        *stack.Stack
	fakeIP    *FakeIPPool
	dnsHijack *DNSHijacker
	routeMgr  *RouteManager
	addr      tcpip.Address
	prefixLen int

	mu      sync.Mutex
	running bool
	closeCh chan struct{}
	wg      sync.WaitGroup

	defaultIfaceName  string
	defaultIfaceIndex int

	logMu sync.Mutex
	logs  []string
}

// NewEngine creates a new TUN engine. It does not start anything yet.
func NewEngine(ruleConf *config.RuleConfiguration) *Engine {
	return &Engine{
		ruleConf: ruleConf,
		closeCh:  make(chan struct{}),
	}
}

// IsEnabled reports whether the TUN engine is active.
func (e *Engine) IsEnabled() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running
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

// Start brings up the TUN device, configures routes, and starts netstack.
func (e *Engine) Start() error {
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return fmt.Errorf("tun engine already running")
	}
	e.mu.Unlock()

	// 0. Ensure admin privileges (Windows UAC auto-elevation)
	if err := EnsureAdminPrivileges(); err != nil {
		e.logEvent("TUN ensure admin privileges failed: %v", err)
		return err
	}

	// 0.5. Clean up residual resources from previous abnormal exit
	CleanupResidual()

	// 1. Create TUN device
	dev, err := CreateDevice()
	if err != nil {
		e.logEvent("TUN create device failed: %v", err)
		return fmt.Errorf("tun: create device: %w", err)
	}
	e.device = dev

	// 2. Pick TUN address (e.g. 198.18.0.1/15 for routing, plus a local addr)
	tunIP := net.ParseIP("198.18.0.1").To4()
	e.addr = tcpip.AddrFrom4([4]byte(tunIP))
	e.prefixLen = 15

	// 3. Create netstack
	if err := e.initStack(); err != nil {
		dev.Close()
		return fmt.Errorf("tun: init netstack: %w", err)
	}

	// 4. Init Fake-IP pool
	e.fakeIP = NewFakeIPPool()

	// 5. Init DNS hijacker
	e.dnsHijack = NewDNSHijacker(e.ns, e.fakeIP)
	if err := e.dnsHijack.Start(&e.wg); err != nil {
		e.dnsHijack.Stop()
		dev.Close()
		e.ns.Close()
		return fmt.Errorf("tun: start dns hijacker: %w", err)
	}

	// 6. Resolve proxy server IPs before TUN takes over DNS/routing,
	//    plus LAN/private subnets that should bypass TUN.
	proxyIPs := e.resolveProxyIPs()
	exclusions := append(proxyIPs, DefaultLANExclusions...)

	// 7. Configure routes
	e.routeMgr = NewRouteManager(dev.Name(), dev.GUID())
	e.routeMgr.SetExclusions(exclusions)
	if err := e.routeMgr.Setup(tunIP.String(), e.prefixLen); err != nil {
		e.logEvent("TUN setup routes failed: %v", err)
		e.dnsHijack.Stop()
		dev.Close()
		e.ns.Close()
		return fmt.Errorf("tun: setup routes: %w", err)
	}

	// Save original interface info for DIRECT connection bypass
	e.defaultIfaceName = e.routeMgr.DefaultIfaceName
	e.defaultIfaceIndex = e.routeMgr.DefaultIfaceIndex

	// 8. Start packet forward loops
	e.mu.Lock()
	e.running = true
	e.closeCh = make(chan struct{})
	e.mu.Unlock()

	e.wg.Add(3)
	go e.readLoop()
	go e.acceptTCP()
	go e.acceptUDP()

	e.logEvent("TUN engine started on %s", dev.Name())
	util.LogInfo("tun engine started on %s", dev.Name())
	return nil
}

// Stop tears down the TUN engine and restores routes.
//
// Order rationale:
//  1. Close closeCh — signal goroutines to exit
//  2. Close device — ends Wintun session (unblocks Read), deletes adapter
//  3. Wait goroutines — they drain after device closes
//  4. Teardown routes — best-effort; may partially fail after adapter removal
//     but CleanupResidual at next Start() catches stragglers
func (e *Engine) Stop() error {
	e.mu.Lock()
	if !e.running {
		e.mu.Unlock()
		return nil
	}
	e.running = false
	close(e.closeCh)
	e.mu.Unlock()

	// 1. Close device first to unblock readLoop (stuck on ReceivePacket)
	//    This also ends the Wintun session and deletes the adapter.
	if e.device != nil {
		e.device.Close()
	}

	// 2. Wait for goroutines to finish
	e.wg.Wait()

	// 3. Best-effort route teardown (adapter may already be gone)
	if e.routeMgr != nil {
		e.routeMgr.Teardown()
	}

	// 4. Stop services
	if e.dnsHijack != nil {
		e.dnsHijack.Stop()
	}
	if e.ns != nil {
		e.ns.Close()
	}

	e.logEvent("TUN engine stopped")
	util.LogInfo("tun engine stopped")
	return nil
}

// initStack creates the gvisor netstack and attaches the link endpoint.
func (e *Engine) initStack() error {
	linkEP := channel.New(512, 1500, "")
	e.linkEP = linkEP

	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	e.ns = s

	const nicID = 1
	if err := s.CreateNIC(nicID, linkEP); err != nil {
		return fmt.Errorf("create nic: %v", err)
	}

	ap := tcpip.AddressWithPrefix{Address: e.addr, PrefixLen: e.prefixLen}
	protoAddr := tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: ap,
	}
	if err := s.AddProtocolAddress(nicID, protoAddr, stack.AddressProperties{}); err != nil {
		return fmt.Errorf("add address: %v", err)
	}

	s.SetPromiscuousMode(nicID, true)
	s.SetSpoofing(nicID, true)

	s.SetRouteTable([]tcpip.Route{
		{Destination: header.IPv4EmptySubnet, NIC: nicID},
		{Destination: header.IPv6EmptySubnet, NIC: nicID},
	})

	return nil
}

// readLoop reads IP packets from the TUN device and injects them into netstack.
func (e *Engine) readLoop() {
	defer e.wg.Done()
	readBuf := make([]byte, 2048)
	for {
		select {
		case <-e.closeCh:
			return
		default:
		}

		// Read into a temporary buffer first, then copy to a per-packet buffer
		// so netstack owns the data and cannot be overwritten by the next read.
		n, err := e.device.Read(readBuf)
		if err != nil {
			select {
			case <-e.closeCh:
				return
			default:
				util.LogWarn("tun: read error: %v", err)
				continue
			}
		}
		if n == 0 {
			continue
		}

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

		pktBuf := make([]byte, n)
		copy(pktBuf, readBuf[:n])
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(pktBuf)})
		e.linkEP.InjectInbound(proto, pkt)
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
			r.Complete(true)
			return
		}
		defer ep.Close()

		conn := gonet.NewTCPConn(&wq, ep)
		defer conn.Close()

		id := r.ID()
		dstAddr := net.IP(id.LocalAddress.AsSlice()).String()
		dstPort := int(id.LocalPort)

		e.handleConn(conn, dstAddr, dstPort)
	})

	e.ns.SetTransportProtocolHandler(tcp.ProtocolNumber, fwd.HandlePacket)

	<-e.closeCh
}

// acceptUDP accepts UDP datagrams from netstack and proxies them through
// the proxy chain or direct dial, mirroring the TCP acceptTCP pattern.
func (e *Engine) acceptUDP() {
	defer e.wg.Done()

	fwd := udp.NewForwarder(e.ns, func(r *udp.ForwarderRequest) {
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
	})

	e.ns.SetTransportProtocolHandler(udp.ProtocolNumber, fwd.HandlePacket)

	<-e.closeCh
}

// handleUDP relays UDP datagrams between netstack and the real network via proxy or direct.
// It preserves datagram boundaries by reading/writing one datagram at a time.
func (e *Engine) handleUDP(netstackConn net.Conn, dstAddr string, dstPort int) {
	// Check if this is a Fake-IP: restore original domain
	if domain := e.fakeIP.LookupDomain(dstAddr); domain != "" {
		util.LogInfo("tun: udp fake-ip %s -> %s", dstAddr, domain)
		dstAddr = domain
	}

	connID := util.NextConnID()

	req := config.NewConnectRequest(dstAddr, dstPort)
	var proxy *config.Proxy
	if e.ruleConf != nil {
		req = e.ruleConf.Resolving(req)
		proxy = e.ruleConf.Match(req, nil)
	}

	resolvedAddr := req.DstAddr
	resolvedPort := req.DstPort

	if proxy != nil && strings.ToUpper(proxy.Type) == config.ProxyREJECT {
		util.LogInfo("[TUN] [%s] udp %s:%d -> REJECTED", connID, resolvedAddr, resolvedPort)
		return
	}

	var targetConn net.PacketConn
	var err error

	if proxy != nil && strings.ToUpper(proxy.Type) != config.ProxyDIRECT {
		targetConn, err = dialer.ChainUDPDial(proxy)
		if err != nil {
			util.LogWarn("[TUN] [%s] udp dial %s:%d via %s fail: %v", connID, resolvedAddr, resolvedPort, proxy.Name, err)
			return
		}
	} else {
		targetConn, err = e.directDialPacket()
		if err != nil {
			util.LogWarn("[TUN] [%s] udp direct dial %s:%d fail: %v", connID, resolvedAddr, resolvedPort, err)
			return
		}
	}
	defer targetConn.Close()

	dstUDPAddr := &net.UDPAddr{IP: net.ParseIP(resolvedAddr), Port: resolvedPort}
	util.LogInfo("[TUN] [%s] udp %s:%d -> %s", connID, resolvedAddr, resolvedPort, proxyDesc(proxy))

	relayUDP(netstackConn, targetConn, dstUDPAddr)
}

// directDialPacket creates a UDP socket bound to the original physical interface
// so DIRECT UDP traffic bypasses the TUN split-tunnel routes.
func (e *Engine) directDialPacket() (net.PacketConn, error) {
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			return setDirectSocketOption(c, e.defaultIfaceName, e.defaultIfaceIndex)
		},
	}
	return lc.ListenPacket(context.Background(), "udp", "")
}

// relayUDP copies datagrams between the netstack UDP connection and the target
// PacketConn, preserving datagram boundaries.
func relayUDP(netstackConn net.Conn, targetConn net.PacketConn, dstAddr *net.UDPAddr) {
	const bufSize = 65535

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
		if _, err := netstackConn.Write(buf[:n]); err != nil {
			break
		}
	}
	wg.Wait()
}

// handleConn routes a TUN-side TCP connection through the proxy chain or direct.
func (e *Engine) handleConn(conn net.Conn, dstAddr string, dstPort int) {
	defer conn.Close()

	// Check if this is a Fake-IP: restore original domain
	if domain := e.fakeIP.LookupDomain(dstAddr); domain != "" {
		util.LogInfo("tun: fake-ip %s -> %s", dstAddr, domain)
		dstAddr = domain
	}

	connID := util.NextConnID()

	req := config.NewConnectRequest(dstAddr, dstPort)
	var proxy *config.Proxy
	if e.ruleConf != nil {
		req = e.ruleConf.Resolving(req)
		proxy = e.ruleConf.Match(req, nil)
	}

	// Use the (possibly redirected) destination from Resolving
	resolvedAddr := req.DstAddr
	resolvedPort := req.DstPort

	var targetConn net.Conn
	var err error

	if proxy != nil && strings.ToUpper(proxy.Type) == config.ProxyREJECT {
		util.LogInfo("[TUN] [%s] %s:%d -> REJECTED", connID, resolvedAddr, resolvedPort)
		return
	}

	if proxy != nil && strings.ToUpper(proxy.Type) != config.ProxyDIRECT {
		targetConn, err = dialer.ChainDialWithID(proxy, resolvedAddr, resolvedPort, connID)
		if err != nil {
			util.LogWarn("[TUN] [%s] dial %s:%d via %s fail: %v", connID, resolvedAddr, resolvedPort, proxy.Name, err)
			return
		}
	} else {
		targetConn, err = e.directDial("tcp", net.JoinHostPort(resolvedAddr, fmt.Sprintf("%d", resolvedPort)))
		if err != nil {
			util.LogWarn("[TUN] [%s] direct dial %s:%d fail: %v", connID, resolvedAddr, resolvedPort, err)
			return
		}
	}
	defer targetConn.Close()

	util.LogInfo("[TUN] [%s] %s:%d -> %s", connID, resolvedAddr, resolvedPort, proxyDesc(proxy))
	util.Relay(conn, targetConn)
}

func proxyDesc(p *config.Proxy) string {
	if p == nil || strings.EqualFold(p.Type, config.ProxyDIRECT) {
		return "DIRECT"
	}
	return p.Name
}

// directDial creates a socket bound to the original physical interface to
// bypass TUN split-tunnel routes (0/1 + 128/1). This avoids routing loops
// where DIRECT traffic re-enters the TUN device.
func (e *Engine) directDial(network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("direct: parse addr: %w", err)
	}

	// If already an IP, dial directly with socket option
	if ip := net.ParseIP(host); ip != nil {
		d := &net.Dialer{
			Timeout: 30 * time.Second,
			Control: func(network, address string, c syscall.RawConn) error {
				return setDirectSocketOption(c, e.defaultIfaceName, e.defaultIfaceIndex)
			},
		}
		return d.Dial(network, addr)
	}

	// Resolve domain via original interface to get real IP (bypasses Fake-IP DNS)
	ips, err := e.resolveDirect(host)
	if err != nil {
		return nil, fmt.Errorf("direct: resolve %s: %w", host, err)
	}

	d := &net.Dialer{
		Timeout: 30 * time.Second,
		Control: func(network, address string, c syscall.RawConn) error {
			return setDirectSocketOption(c, e.defaultIfaceName, e.defaultIfaceIndex)
		},
	}
	return d.Dial(network, net.JoinHostPort(ips[0], port))
}

// resolveDirect resolves a domain name via the original physical interface,
// bypassing the TUN Fake-IP DNS hijacker.
func (e *Engine) resolveDirect(host string) ([]string, error) {
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{
				Control: func(network, address string, c syscall.RawConn) error {
					return setDirectSocketOption(c, e.defaultIfaceName, e.defaultIfaceIndex)
				},
			}
			return d.DialContext(ctx, network, address)
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return r.LookupHost(ctx, host)
}

// resolveProxyIPs extracts and resolves proxy server addresses before TUN takes
// over DNS/routing, so we can add exclusion routes for them.
//
// We exclude all configured proxy servers rather than trying to compute the
// "first hop" of a proxy chain, because not every dialer honors proxy.Next
// (e.g. some protocols connect directly to their own server). Over-exclusion
// is harmless; under-exclusion would let a first-hop connection re-enter TUN.
// Resolution is done concurrently to avoid startup delays from slow/unreachable hosts.
func (e *Engine) resolveProxyIPs() []string {
	if e.ruleConf == nil {
		return nil
	}

	// Collect unique hostnames to resolve
	hostSet := make(map[string]struct{})
	addHost := func(host string) {
		if host == "" {
			return
		}
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if net.ParseIP(host) != nil {
			return // already an IP, skip DNS
		}
		hostSet[host] = struct{}{}
	}

	e.ruleConf.RLock()
	for _, p := range e.ruleConf.Proxies {
		if !p.IsEnabled() {
			continue
		}
		addHost(p.Server)
		addHost(p.Sni)
		addHost(p.Servername)
	}
	for _, m := range e.ruleConf.Mappings {
		if !m.IsEnabled() {
			continue
		}
		addHost(m.ReverseAddress)
	}
	e.ruleConf.RUnlock()

	if len(hostSet) == 0 {
		return nil
	}

	// Concurrent DNS resolution
	type result struct {
		host string
		ips  []string
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	results := make([]result, 0, len(hostSet))

	for hostname := range hostSet {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			addrs, err := net.LookupHost(h)
			if err != nil {
				util.LogWarn("tun: failed to resolve proxy host %s: %v", h, err)
				return
			}
			var ips []string
			for _, addr := range addrs {
				if net.ParseIP(addr) != nil {
					ips = append(ips, addr)
				}
			}
			if len(ips) > 0 {
				mu.Lock()
				results = append(results, result{host: h, ips: ips})
				mu.Unlock()
			}
		}(hostname)
	}
	wg.Wait()

	// Deduplicate IPs across all resolved hosts
	ipSet := make(map[string]struct{})
	for _, r := range results {
		for _, ip := range r.ips {
			ipSet[ip] = struct{}{}
		}
	}

	var ips []string
	for ip := range ipSet {
		ips = append(ips, ip)
	}
	if len(ips) > 0 {
		util.LogInfo("tun: %d proxy server IPs to exclude from TUN", len(ips))
	}
	return ips
}
