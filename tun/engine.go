package tun

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
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
	dnsAddr   tcpip.Address
	prefixLen int

	mu      sync.Mutex
	running bool
	closeCh chan struct{}
	wg      sync.WaitGroup

	// packet counters for diagnostics
	readPackets  atomic.Uint64
	writePackets atomic.Uint64
	dnsProxy     *DNSProxy

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

// Start brings up the TUN device, configures routes, and starts netstack.
func (e *Engine) Start() error {
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return fmt.Errorf("tun engine already running")
	}

	// 0. Ensure admin privileges (Windows UAC auto-elevation)
	if err := EnsureAdminPrivileges(); err != nil {
		e.mu.Unlock()
		e.logEvent("TUN ensure admin privileges failed: %v", err)
		return err
	}

	// 0.5. Clean up residual resources from previous abnormal exit
	CleanupResidual()

	// 1. Create TUN device
	dev, err := CreateDevice()
	if err != nil {
		e.mu.Unlock()
		e.logEvent("TUN create device failed: %v", err)
		return fmt.Errorf("tun: create device: %w", err)
	}
	e.device = dev

	// 2. Pick TUN addresses.
	// hostIP is the address assigned to the Windows Wintun adapter itself;
	// it must NOT be added as a local netstack address, otherwise replies
	// destined to it from the DNS hijacker / forwarders would be looped back
	// inside netstack instead of being written back to the Wintun device.
	// dnsIP is the internal DNS hijacker address. It is kept off the TUN subnet
	// so that locally-originated DNS queries are delivered to the hijacker
	// inside netstack rather than routed out the Wintun device.
	hostIP := net.ParseIP("192.0.2.2").To4()
	dnsIP := net.ParseIP("127.0.0.1").To4()
	e.addr = tcpip.AddrFrom4([4]byte(hostIP))
	e.dnsAddr = tcpip.AddrFrom4([4]byte(dnsIP))
	e.prefixLen = 30

	// 3. Create netstack
	if err := e.initStack(); err != nil {
		dev.Close()
		e.mu.Unlock()
		return fmt.Errorf("tun: init netstack: %w", err)
	}

	// 4. Init Fake-IP pool
	e.fakeIP = NewFakeIPPoolWithStack(e.ns, tunNICID)

	// 5. Init DNS hijacker
	e.dnsHijack = NewDNSHijacker(e.ns, e.fakeIP, e.addr, e.dnsAddr)
	if err := e.dnsHijack.Start(&e.wg); err != nil {
		e.dnsHijack.Stop()
		e.wg.Wait()
		dev.Close()
		e.ns.Close()
		e.mu.Unlock()
		return fmt.Errorf("tun: start dns hijacker: %w", err)
	}

	// 6. LAN/private subnets should bypass TUN to avoid breaking local network
	//    connectivity. Proxy server exclusion routes are intentionally omitted:
	//    outbound sockets are bound to the correct physical interface by the
	//    dialer package, so proxy traffic does not loop back into TUN.
	e.routeMgr = NewRouteManager(dev.Name(), dev.GUID())
	// On Windows the Wintun adapter LUID is available immediately; passing it in
	// avoids waiting for the TCP/IP stack to register the adapter by name.
	if luidGetter, ok := dev.(interface{ LUID() uint64 }); ok {
		e.routeMgr.SetTUNLUID(luidGetter.LUID())
	}
	e.routeMgr.SetExclusions(DefaultLANExclusions)
	if err := e.routeMgr.Setup(hostIP.String(), e.prefixLen); err != nil {
		e.logEvent("TUN setup routes failed: %v", err)
		e.dnsHijack.Stop()
		e.wg.Wait()
		dev.Close()
		e.ns.Close()
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

	// 7. Start packet forward loops before exposing the TUN DNS path to the
	//    system, so queries that arrive immediately after the system DNS redirect
	//    are handled by the netstack and DNS hijacker.
	e.running = true
	e.closeCh = make(chan struct{})
	e.mu.Unlock()

	e.wg.Add(4)
	go e.readLoop()
	go e.writeLoop()
	go e.acceptTCP()
	go e.acceptUDP()

	// Diagnostic goroutine: log packet counts every 5 seconds.
	e.wg.Add(1)
	go e.logPacketCounts()

	// 8. Start a Windows-side DNS proxy on the TUN adapter IP. System DNS is set
	//    to the adapter IP, so Windows delivers queries to this local socket;
	//    the proxy forwards them into the netstack DNS hijacker.
	e.dnsProxy = NewDNSProxy(e)
	if err := e.dnsProxy.Start(); err != nil {
		util.LogWarn("tun: failed to start DNS proxy: %v", err)
	}

	// 9. Redirect system DNS to the TUN adapter IP so applications send queries
	//    to the local DNS proxy.
	if err := setSystemDNS(dev.Name(), hostIP.String()); err != nil {
		util.LogWarn("tun: failed to set system dns: %v", err)
	}

	// Engine health watchdog is intentionally disabled.
	//
	// A watchdog running inside the phaethon process cannot reliably probe the
	// TUN DNS path: packets originated by the service process itself are not
	// looped back through the wintun adapter to the same process, so both
	// system-resolver and internal-netstack probes time out. The external
	// watchdog process (spawned via LAYER_WATCHDOG_PID) still monitors parent
	// death and cleans up routes/DNS to prevent a stranded broken network.
	// e.watchdog = NewHealthWatchdog(e)
	// e.watchdog.Start()

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

	// Clear the global bind context so subsequent dials resume normal behavior.
	dialer.SetGlobalBindContext(nil)

	// Stop the Windows-side DNS proxy so queries are not answered after system
	// DNS is restored.
	if e.dnsProxy != nil {
		e.dnsProxy.Stop()
		e.dnsProxy = nil
	}

	// 1. Restore system DNS first while the TUN adapter still exists.
	if e.device != nil {
		restoreSystemDNS(e.device.Name())
	}

	// 2. Teardown routes while the adapter still has a valid LUID/index.
	if e.routeMgr != nil {
		e.routeMgr.Teardown()
	}

	// 3. Close device to unblock readLoop (stuck on ReceivePacket)
	//    This also ends the Wintun session and deletes the adapter.
	if e.device != nil {
		e.device.Close()
	}

	// 4. Wait for goroutines to finish
	e.wg.Wait()

	// 5. Stop services
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
const tunNICID = 1

func (e *Engine) initStack() error {
	linkEP := channel.New(512, 1500, "")
	e.linkEP = linkEP

	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	e.ns = s

	if err := s.CreateNIC(tunNICID, linkEP); err != nil {
		return fmt.Errorf("create nic: %v", err)
	}

	ap := tcpip.AddressWithPrefix{Address: e.dnsAddr, PrefixLen: 8}
	protoAddr := tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: ap,
	}
	if err := s.AddProtocolAddress(tunNICID, protoAddr, stack.AddressProperties{}); err != nil {
		return fmt.Errorf("add dns address: %v", err)
	}

	// Do NOT add 192.0.2.2/30 as a local netstack address. The TUN adapter IP
	// is configured on the Windows side so the host uses it as the source
	// address for packets entering Wintun. If netstack considered it local,
	// DNS responses (and other replies) destined to 192.0.2.2 would be
	// looped back inside netstack instead of being written back to the Wintun
	// device for the host resolver to receive.

	s.SetPromiscuousMode(tunNICID, true)
	s.SetSpoofing(tunNICID, true)

	// Allow the netstack to forward IPv4/IPv6 packets that are not addressed to
	// a local NIC address. This is required for the Fake-IP scheme: connections
	// to 198.18.0.0/15 are routed to the TUN NIC and must be handled by the
	// TCP/UDP forwarders even though the destination is not assigned locally.
	_ = s.SetForwardingDefaultAndAllNICs(ipv4.ProtocolNumber, true)
	_ = s.SetForwardingDefaultAndAllNICs(ipv6.ProtocolNumber, true)

	s.SetRouteTable([]tcpip.Route{
		{Destination: header.IPv4EmptySubnet, NIC: tunNICID},
		{Destination: header.IPv6EmptySubnet, NIC: tunNICID},
	})

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
				if errors.Is(err, ErrSessionClosed) {
					util.LogError("tun: session closed, stopping read loop: %v", err)
					return
				}
				util.LogWarn("tun: read error: %v", err)
				continue
			}
		}
		if n == 0 {
			continue
		}
		e.readPackets.Add(1)

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
			dstIP := net.IP(readBuf[16:20]).String()
			srcIP := net.IP(readBuf[12:16]).String()
			ipProto := readBuf[9]
			if strings.HasPrefix(dstIP, "198.18.") || strings.HasPrefix(dstIP, "198.19.") {
				util.LogInfo("tun read FAKE: %s -> %s (proto=%d len=%d cnt=%d)", srcIP, dstIP, ipProto, n, e.readPackets.Load())
			} else if e.readPackets.Load() <= 200 {
				util.LogInfo("tun read: %s -> %s (proto=%d len=%d)", srcIP, dstIP, ipProto, n)
			}
		}

		pktBuf := make([]byte, n)
		copy(pktBuf, readBuf[:n])
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(pktBuf)})
		e.linkEP.InjectInbound(proto, pkt)
		pkt.DecRef()
	}
}

// writeLoop reads outbound packets from netstack and writes them to the TUN device.
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

		if e.device == nil {
			pkt.DecRef()
			continue
		}

		buf := pkt.ToBuffer()
		data := buf.Flatten()

		// Log outbound packets for debugging. Fake-IP replies and packets to the
		// TUN host IP are always logged; everything else is logged for the first
		// 200 packets to cap noise.
		if len(data) >= 20 && (data[0]>>4) == 4 {
			dstIP := net.IP(data[16:20]).String()
			srcIP := net.IP(data[12:16]).String()
			ipProto := data[9]
			isFake := strings.HasPrefix(dstIP, "198.18.") || strings.HasPrefix(dstIP, "198.19.") ||
				strings.HasPrefix(srcIP, "198.18.") || strings.HasPrefix(srcIP, "198.19.")
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
	var dialIP net.IP

	if proxy != nil && strings.ToUpper(proxy.Type) != config.ProxyDIRECT {
		targetConn, err = dialer.ChainUDPDial(proxy)
		if err != nil {
			util.LogWarn("[TUN] [%s] udp dial %s:%d via %s fail: %v", connID, resolvedAddr, resolvedPort, proxy.Name, err)
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
			return
		}
	}
	defer targetConn.Close()

	// Use the resolved IP for the destination address.
	dstUDPAddr := &net.UDPAddr{IP: dialIP, Port: resolvedPort}
	util.LogInfo("[TUN] [%s] udp %s:%d -> %s", connID, resolvedAddr, resolvedPort, proxyDesc(proxy))

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
		// Direct dial: resolve real IP now if we have a domain.
		dialAddr := resolvedAddr
		if domain != "" {
			// Resolve the real IP for DIRECT connections.
			ips, err := e.resolveForDirect(domain)
			if err != nil || len(ips) == 0 {
				util.LogWarn("[TUN] [%s] resolve %s fail: %v", connID, domain, err)
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

	remoteAddr := tcpip.FullAddress{NIC: tunNICID, Addr: e.dnsAddr, Port: 53}
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
