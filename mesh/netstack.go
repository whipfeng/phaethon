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

	// Multi-NIC architecture (Phase 2+)
	meshEP    *MeshEndpoint    // NIC 2: Mesh endpoint for mesh traffic
	meshMgr   *MeshManager     // Mesh manager for routing decisions
	tunnelNICs map[string]*TunnelNIC // NIC 201+: Per-node tunnel NICs for IPIP
	tunnelNextNICID atomic.Uint64     // Next NIC ID for tunnel NICs (starting at 201)

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

	// Mesh subnet for writeLoop routing decisions
	meshSubnet *net.IPNet

	// Mesh-related callbacks (interceptor removed in phase 2)
	isLocalMeshVIP  func(ip net.IP) bool
	isMeshIPFunc    func(ip net.IP) bool

	// FakeIP reverse lookup for diagnostics
	lookupDomainFunc func(ip string) string

	// TCP keepalive settings
	tcpKeepalive *config.MeshTCPKeepalive

	// Packet counters for diagnostics
	ReadPackets  atomic.Uint64
	WritePackets atomic.Uint64
}

// NewNetstack creates a new Netstack instance.
func NewNetstack() *Netstack {
	return &Netstack{
		htunnelEndpoints: make(map[string]*HTunnelEndpoint),
		tunnelNICs:       make(map[string]*TunnelNIC),
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

// MeshEP returns the mesh link endpoint (NIC 2).
func (n *Netstack) MeshEP() *MeshEndpoint {
	return n.meshEP
}

// SetMeshManager sets the mesh manager on the mesh endpoint for sending packets.
func (n *Netstack) SetMeshManager(meshMgr *MeshManager) {
	n.meshMgr = meshMgr // Store for RouteSelector
	if n.meshEP != nil {
		n.meshEP.SetMeshManager(meshMgr)
	}
	// Set route change callback to update tunnel NICs
	if meshMgr != nil {
		meshMgr.SetOnRouteChange(func() {
			if err := n.UpdateTunnelNICs(meshMgr); err != nil {
				util.LogError("[NETSTACK] failed to update tunnel NICs on route change: %v", err)
			}
		})
		// Initial tunnel NIC update
		if err := n.UpdateTunnelNICs(meshMgr); err != nil {
			util.LogError("[NETSTACK] failed initial tunnel NIC update: %v", err)
		}

		// Configure RouteSelector for dynamic routing decisions
		// RouteSelector is called during FindRoute to handle:
		// 1. fakeIP → LocalDelivery (to Forwarder for domain resolution)
		// 2. Mesh subnet → empty decision (route table handles direct peer routes)
		// 3. Non-mesh → IPIP encapsulation with egress node VIP
		if n.ns != nil && n.meshSubnet != nil {
			// Calculate local VIP and EIP
			localVIP := CalculateVIP(n.meshSubnet)
			localEIP := CalculateEIP(n.meshSubnet)

			// Get FakeIP pool for fakeIP checking
			fakeIPPool := meshMgr.GetFakeIPPool()

			// Create RouteSelector configuration
			routeSelectorCfg := &RouteSelectorConfig{
				MeshSubnet: n.meshSubnet,
				LocalVIP:   tcpip.AddrFrom4Slice(localVIP),
				LocalEIP:   tcpip.AddrFrom4Slice(localEIP),
				IsFakeIP: func(ip net.IP) bool {
					if fakeIPPool == nil {
						return false
					}
					return fakeIPPool.Contains(ip)
				},
				SelectEgressNode: func(dst net.IP) (tcpip.Address, bool) {
					// Get route table
					rt := meshMgr.GetRouteTable()
					if rt == nil {
						return tcpip.Address{}, false
					}

					// Find matching route entry
					for _, route := range rt.Routes {
						if route.Prefix.Contains(dst) {
							// Select egress node for this prefix
							if len(route.Entries) == 0 {
								continue
							}
							targetNodeID := meshMgr.SelectEgressNodeID(dst, route.Entries)
							if targetNodeID == "" {
								continue
							}

							// Get VIP for the egress node
							targetVIP := meshMgr.GetVIPForNode(targetNodeID)
							if targetVIP == nil {
								continue
							}

							return tcpip.AddrFrom4Slice(targetVIP), true
						}
					}

					return tcpip.Address{}, false
				},
			}

			// Create and set RouteSelector
			routeSelector := NewRouteSelector(routeSelectorCfg)
			n.ns.SetRouteSelector(routeSelector)
			util.LogInfo("netstack: RouteSelector configured (VIP=%s, EIP=%s)", localVIP, localEIP)
		}
	}
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

// SetLocalMeshVIPFunc sets the function to check if an IP is a local mesh VIP.
func (n *Netstack) SetLocalMeshVIPFunc(f func(ip net.IP) bool) {
	n.isLocalMeshVIP = f
}

// SetIsMeshIPFunc sets the function to check if an IP belongs to the mesh network.
func (n *Netstack) SetIsMeshIPFunc(f func(ip net.IP) bool) {
	n.isMeshIPFunc = f
}

// SetLookupDomainFunc sets the function to look up domain name from FakeIP.
func (n *Netstack) SetLookupDomainFunc(f func(ip string) string) {
	n.lookupDomainFunc = f
}

// SetCallbacks sets the forwarder callbacks.
func (n *Netstack) SetCallbacks(cb *ForwarderCallbacks) {
	n.callbacks = cb
}

// SetTCPKeepalive sets the TCP keepalive settings.
func (n *Netstack) SetTCPKeepalive(ka *config.MeshTCPKeepalive) {
	n.tcpKeepalive = ka
}

// applyTCPKeepalive applies TCP keepalive settings to an endpoint.
func (n *Netstack) applyTCPKeepalive(ep tcpip.Endpoint) {
	kaIdle := n.tcpKeepalive.GetIdle()
	kaInterval := n.tcpKeepalive.GetInterval()
	kaCount := n.tcpKeepalive.GetCount()
	ep.SocketOptions().SetKeepAlive(true)
	idle := tcpip.KeepaliveIdleOption(time.Duration(kaIdle) * time.Second)
	if err := ep.SetSockOpt(&idle); err != nil {
		util.LogDebug("[TCP] failed to set keepalive idle: %v", err)
	}
	interval := tcpip.KeepaliveIntervalOption(time.Duration(kaInterval) * time.Second)
	if err := ep.SetSockOpt(&interval); err != nil {
		util.LogDebug("[TCP] failed to set keepalive interval: %v", err)
	}
	count := tcpip.SockOptInt(kaCount)
	if err := ep.SetSockOptInt(tcpip.KeepaliveCountOption, int(count)); err != nil {
		util.LogDebug("[TCP] failed to set keepalive count: %v", err)
	}
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

// initStack creates the gvisor netstack with multiple NICs.
// NIC 1: TUN adapter (channel.Endpoint) - TUN device I/O, NAT at boundary
// NIC 2: Mesh endpoint - mesh traffic, bound to GIP
// writeLoop handles all routing decisions: VIP/hostIP -> TUN, mesh -> mesh link, other -> re-inject.
func (n *Netstack) initStack() error {
	// NIC 1: TUN adapter (channel endpoint)
	linkEP := channel.New(8192, 1500, "")
	n.linkEP = linkEP

	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, func(s *stack.Stack) stack.TransportProtocol { return newIPIPProtocol(s, linkEP) }},
	})
	n.ns = s

	if err := s.CreateNIC(1, linkEP); err != nil {
		return fmt.Errorf("create nic 1: %v", err)
	}

	// NIC 1: TUN adapter - receive-only + writeLoop for external NAT return
	// Promiscuous=true: accept all inbound packets (raw-IP, transit traffic)
	// Spoofing=true: allow packets with external source IPs from Forwarder
	// Forwarding=true: stack forwards packets (required for iptables SNAT)
	s.SetPromiscuousMode(1, true)
	_ = s.SetSpoofing(1, true)
	_ = s.SetForwardingDefaultAndAllNICs(ipv4.ProtocolNumber, true)
	_ = s.SetForwardingDefaultAndAllNICs(ipv6.ProtocolNumber, true)
	s.SetNICName(1, "tun")

	// NIC 2: Mesh endpoint - handles mesh traffic, bound to GIP
	meshEP := NewMeshEndpoint(1500)
	n.meshEP = meshEP

	if err := s.CreateNIC(2, meshEP); err != nil {
		return fmt.Errorf("create nic 2 (mesh): %v", err)
	}

	// Bind GIP (.3) to NIC 2
	gipAddr := tcpip.AddressWithPrefix{Address: n.dnsAddr, PrefixLen: 32}
	if err := s.AddProtocolAddress(2, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: gipAddr,
	}, stack.AddressProperties{}); err != nil {
		return fmt.Errorf("add GIP address to NIC 2: %v", err)
	}

	// Bind EIP (.4) to NIC 2 (design requirement D6)
	// EIP is used for IPIP tunnel decapsulation - the stack matches outer dst=EIP
	if n.meshSubnet != nil {
		eipIP := make(net.IP, 4)
		copy(eipIP, n.meshSubnet.IP.To4())
		eipIP[3] = eipIP[3] + 4 // EIP = subnet + 4
		eipAddr := tcpip.AddressWithPrefix{Address: tcpip.AddrFrom4Slice(eipIP), PrefixLen: 32}
		if err := s.AddProtocolAddress(2, tcpip.ProtocolAddress{
			Protocol:          ipv4.ProtocolNumber,
			AddressWithPrefix: eipAddr,
		}, stack.AddressProperties{}); err != nil {
			util.LogWarn("[NETSTACK] failed to add EIP address to NIC 2: %v (IPIP decapsulation may not work)", err)
		} else {
			util.LogInfo("[NETSTACK] bound EIP=%s to NIC 2 for IPIP decapsulation", eipIP)
		}
	}

	// NIC 2: Promiscuous=false (only accepts packets destined for GIP)
	// Spoofing=true: allow packets with GIP as source
	s.SetSpoofing(2, true)
	s.SetNICName(2, "mesh")

	// Route table:
	// - VIP (100.x.0.1) → NIC 1 (external NAT return → writeLoop → TUN)
	// - Mesh network (100.0.0.0/8) → NIC 2 (other mesh nodes)
	routes := []tcpip.Route{}

	// Add mesh routes if available
	if n.meshSubnet != nil {
		// Calculate VIP (subnet + 1)
		vipIP := make(net.IP, 4)
		copy(vipIP, n.meshSubnet.IP.To4())
		vipIP[3] = vipIP[3] + 1

		vipAddr := tcpip.AddrFrom4Slice(vipIP)

		// Mesh network (100.0.0.0/8) → NIC 2
		meshNetwork := tcpip.AddressWithPrefix{
			Address:   tcpip.AddrFrom4Slice(n.meshSubnet.IP.To4()),
			PrefixLen: 8,
		}

		// VIP (/32) → NIC 1
		vipRoute := tcpip.Route{
			Destination: tcpip.AddressWithPrefix{Address: vipAddr, PrefixLen: 32}.Subnet(),
			NIC:         1,
		}

		routes = append(routes,
			vipRoute, // VIP → NIC 1
			tcpip.Route{Destination: meshNetwork.Subnet(), NIC: 2}, // mesh /8 → NIC 2
		)

		util.LogInfo("Setting route table: VIP=%s (NIC 1), mesh=%s (NIC 2)",
			vipAddr, meshNetwork.Subnet())
	} else {
		util.LogInfo("Setting route table: empty - mesh subnet not configured")
	}

	s.SetRouteTable(routes)

	// Configure iptables SNAT for bypass gateway
	// Packets from TUN (NIC 1) going to external networks need SNAT: src → VIP
	// conntrack automatically handles reverse NAT (DNAT) for return packets
	// Patch #4 enables InputInterface matching in Postrouting hook
	if n.meshSubnet != nil {
		// Calculate VIP (subnet + 1)
		vipIP := make(net.IP, 4)
		copy(vipIP, n.meshSubnet.IP.To4())
		vipIP[3] = vipIP[3] + 1
		vipAddr := tcpip.AddrFrom4Slice(vipIP)

		// Create NAT table with Postrouting hook
		// Rule: packets from NIC 1 (TUN) in Postrouting, SNAT src to VIP
		// InputInterface filter ensures only bypass gateway traffic is SNATed
		natTable := stack.Table{
			Rules: []stack.Rule{
				{
					// Filter: match packets from NIC 1 (TUN adapter)
					Filter: stack.IPHeaderFilter{
						InputInterface:      "nic1",
						InputInterfaceInvert: false,
					},
					Target: &stack.SNATTarget{
						Addr:            vipAddr,
						NetworkProtocol: ipv4.ProtocolNumber,
						ChangeAddress:   true,
					},
				},
			},
			BuiltinChains: [stack.NumHooks]int{
				stack.Prerouting:  stack.HookUnset,
				stack.Input:       stack.HookUnset,
				stack.Forward:     stack.HookUnset,
				stack.Output:      stack.HookUnset,
				stack.Postrouting: 0, // Entry point to rules
			},
			Underflows: [stack.NumHooks]int{
				stack.Prerouting:  stack.HookUnset,
				stack.Input:       stack.HookUnset,
				stack.Forward:     stack.HookUnset,
				stack.Output:      stack.HookUnset,
				stack.Postrouting: stack.HookUnset,
			},
		}

		// Replace NAT table (ipv4=false means IPv4)
		s.IPTables().ReplaceTable(stack.NATID, natTable, false /* ipv6 */)
		util.LogInfo("netstack: iptables SNAT configured (VIP=%s, Postrouting InputInterface=nic1)", vipAddr)
	}

	util.LogInfo("netstack: initialized with 2-NIC topology (NIC 1: TUN, NIC 2: Mesh/GIP=%s)", n.dnsAddr)
	return nil
}

// UpdateTunnelNICs creates or updates tunnel NICs for egress nodes and syncs the route table.
// This should be called when mesh routes change (e.g., new advertisements).
func (n *Netstack) UpdateTunnelNICs(meshMgr *MeshManager) error {
	if meshMgr == nil {
		return fmt.Errorf("mesh manager is nil")
	}

	// Get mesh route table
	rt := meshMgr.GetRouteTable()
	if rt == nil {
		util.LogWarn("[NETSTACK] cannot update tunnel NICs: mesh route table is nil")
		return nil
	}

	// Identify egress nodes and their advertised prefixes
	// Egress nodes are nodes that advertise non-mesh routes (outside 100.64.0.0/10)
	egressNodes := make(map[string]net.IP) // nodeID -> EIP
	advertisedRoutes := make([]struct {
		prefix *net.IPNet
		nodeID string
	}, 0)

	meshSubnet := meshMgr.GetMeshSubnet()
	if meshSubnet == nil {
		util.LogWarn("[NETSTACK] cannot update tunnel NICs: mesh subnet not configured")
		return nil
	}

	for _, route := range rt.Routes {
		// Skip mesh routes (100.64.0.0/10)
		if isMeshSubnet(route.Prefix, meshSubnet) {
			continue
		}

		// This is an advertised route (non-mesh)
		// Select the best egress node for this prefix
		if len(route.Entries) == 0 {
			continue
		}

		targetNodeID := meshMgr.SelectEgressNodeID(route.Prefix.IP, route.Entries)
		if targetNodeID == "" {
			continue
		}

		targetEIP := meshMgr.GetEIPForNode(targetNodeID)
		if targetEIP == nil {
			util.LogWarn("[NETSTACK] no EIP for egress node %s, skipping route %s", targetNodeID, route.Prefix)
			continue
		}

		egressNodes[targetNodeID] = targetEIP
		advertisedRoutes = append(advertisedRoutes, struct {
			prefix *net.IPNet
			nodeID string
		}{prefix: route.Prefix, nodeID: targetNodeID})
	}

	// Create tunnel NICs for each egress node
	localEIP := meshMgr.GetIPIPTunnel().GetLocalEIP()
	if localEIP == nil {
		util.LogWarn("[NETSTACK] cannot create tunnel NICs: local EIP not set")
		return nil
	}

	for nodeID, eip := range egressNodes {
		if _, exists := n.tunnelNICs[nodeID]; exists {
			// Tunnel NIC already exists for this node
			continue
		}

		// Create new tunnel NIC
		nicID := tcpip.NICID(201 + n.tunnelNextNICID.Add(1) - 1)
		mtu := uint32(1400) // Conservative MTU for IPIP (1500 - 20 outer header - 80 safety margin)
		tunnelNIC := NewTunnelNIC(nicID, nodeID, tcpip.AddrFrom4Slice(eip), tcpip.AddrFrom4Slice(localEIP), mtu, meshMgr)

		// Register NIC with stack
		if err := n.ns.CreateNIC(nicID, tunnelNIC); err != nil {
			util.LogError("[NETSTACK] failed to create tunnel NIC for node %s: %v", nodeID, err)
			continue
		}

		// Set NIC name for debugging
		n.ns.SetNICName(nicID, fmt.Sprintf("tunnel_%s", nodeID))

		// Enable spoofing (allow packets with any source IP)
		n.ns.SetSpoofing(nicID, true)

		n.tunnelNICs[nodeID] = tunnelNIC
		util.LogInfo("[NETSTACK] created tunnel NIC %d for node %s (EIP=%s)", nicID, nodeID, eip)
	}

	// Build new route table
	routes := []tcpip.Route{}

	// Add VIP route (NIC 1)
	if n.meshSubnet != nil {
		vipIP := make(net.IP, 4)
		copy(vipIP, n.meshSubnet.IP.To4())
		vipIP[3] = vipIP[3] + 1
		vipAddr := tcpip.AddrFrom4Slice(vipIP)
		routes = append(routes, tcpip.Route{
			Destination: tcpip.AddressWithPrefix{Address: vipAddr, PrefixLen: 32}.Subnet(),
			NIC:         1,
		})
	}

	// Add mesh route (NIC 2)
	if n.meshSubnet != nil {
		meshNetwork := tcpip.AddressWithPrefix{
			Address:   tcpip.AddrFrom4Slice(n.meshSubnet.IP.To4()),
			PrefixLen: 8,
		}
		routes = append(routes, tcpip.Route{Destination: meshNetwork.Subnet(), NIC: 2})
	}

	// Add advertised routes (tunnel NICs)
	for _, ar := range advertisedRoutes {
		tunnelNIC, exists := n.tunnelNICs[ar.nodeID]
		if !exists {
			continue
		}

		prefix := tcpip.AddressWithPrefix{
			Address:   tcpip.AddrFrom4Slice(ar.prefix.IP.To4()),
			PrefixLen: prefixLen(ar.prefix),
		}
		routes = append(routes, tcpip.Route{
			Destination: prefix.Subnet(),
			NIC:         tunnelNIC.NICID(),
		})
		util.LogDebug("[NETSTACK] route: %s → tunnel NIC %d (node %s)", ar.prefix, tunnelNIC.NICID(), ar.nodeID)
	}

	// Update route table
	n.ns.SetRouteTable(routes)
	util.LogInfo("[NETSTACK] route table updated: %d routes (%d advertised via tunnel NICs)", len(routes), len(advertisedRoutes))

	return nil
}

// isMeshSubnet checks if a prefix is within the mesh subnet (100.64.0.0/10)
func isMeshSubnet(prefix *net.IPNet, meshSubnet *net.IPNet) bool {
	if prefix == nil || meshSubnet == nil {
		return false
	}
	// Check if the prefix is completely within the mesh subnet
	return meshSubnet.Contains(prefix.IP) && prefixLen(prefix) >= 10
}

// prefixLen returns the prefix length of an IPNet
func prefixLen(ipnet *net.IPNet) int {
	if ipnet == nil {
		return 0
	}
	ones, _ := ipnet.Mask.Size()
	return ones
}

// InjectMeshPacket injects a raw IP packet into the netstack as if received from the mesh network.
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
		ttl := data[8]
		util.LogInfo("[NETSTACK-INJECT] InjectMeshPacket: %s -> %s proto=%d TTL=%d len=%d",
			srcIP, dstIP, data[9], ttl, len(data))
		if ttl == 0 {
			util.LogInfo("[NETSTACK-INJECT] *** TTL=0 packet being injected into gVisor! ***")
		}
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

	// Inject to NIC 2 (Mesh endpoint) instead of NIC 1
	if n.meshEP != nil {
		n.meshEP.DeliverNetworkPacket(data)
		return nil
	}

	// Fallback to NIC 1 if mesh endpoint not available (shouldn't happen)
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

	n.applyTCPKeepalive(ep)
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

	n.applyTCPKeepalive(ep)
	return gonet.NewTCPConn(&wq, ep), nil
}

// NetDialWithModeB dials through the netstack and registers in ModeBTable before sending SYN.
// This ensures the forwarder can find the entry for local loopback cases.
func (n *Netstack) NetDialWithModeB(network, addr string, clientAddr string, inbound string, mapping *config.Mapping, modeBTable *ModeBTable) (net.Conn, error) {
	dialStart := time.Now()
	util.LogInfo("[NETSTACK-DIAL] starting: network=%s addr=%s client=%s inbound=%s", network, addr, clientAddr, inbound)
	n.mu.Lock()
	running := n.running
	ns := n.ns
	n.mu.Unlock()

	if !running || ns == nil {
		util.LogWarn("[NETSTACK-DIAL] failed: netstack not running")
		return nil, fmt.Errorf("netstack not running")
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		util.LogWarn("[NETSTACK-DIAL] failed to parse addr %s: %v", addr, err)
		return nil, fmt.Errorf("netstack dial: parse addr: %w", err)
	}
	portNum, err := net.LookupPort(network, port)
	if err != nil {
		util.LogWarn("[NETSTACK-DIAL] failed to parse port %s: %v", port, err)
		return nil, fmt.Errorf("netstack dial: parse port: %w", err)
	}

	ip := net.ParseIP(host)
	if ip == nil {
		util.LogWarn("[NETSTACK-DIAL] failed: not an IP: %s", host)
		return nil, fmt.Errorf("netstack dial: not an IP: %s", host)
	}
	ip4 := ip.To4()
	if ip4 == nil {
		util.LogWarn("[NETSTACK-DIAL] failed: IPv6 not supported: %s", host)
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
		util.LogWarn("[NETSTACK-DIAL] failed: unsupported network: %s", network)
		return nil, fmt.Errorf("netstack dial: unsupported network: %s", network)
	}

	// Create endpoint and bind
	util.LogDebug("[NETSTACK-DIAL] creating endpoint for %s", addr)
	epCreateStart := time.Now()
	var wq waiter.Queue
	ep, tcpErr := ns.NewEndpoint(tcp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
	if tcpErr != nil {
		util.LogWarn("[NETSTACK-DIAL] failed to create endpoint after %v: %v", time.Since(epCreateStart), tcpErr)
		return nil, fmt.Errorf("create endpoint: %s", tcpErr)
	}
	util.LogDebug("[NETSTACK-DIAL] endpoint created in %v", time.Since(epCreateStart))

	// Bind to GIP with port 0 to allocate source port
	localAddr := tcpip.FullAddress{
		Addr: n.dnsAddr,
		Port: 0,
	}
	bindStart := time.Now()
	if tcpErr := ep.Bind(localAddr); tcpErr != nil {
		util.LogWarn("[NETSTACK-DIAL] failed to bind after %v: %v", time.Since(bindStart), tcpErr)
		ep.Close()
		return nil, fmt.Errorf("bind: %s", tcpErr)
	}

	// Get allocated source port
	localAddr, tcpErr = ep.GetLocalAddress()
	if tcpErr != nil {
		util.LogWarn("[NETSTACK-DIAL] failed to get local address: %v", tcpErr)
		ep.Close()
		return nil, fmt.Errorf("get local address: %s", tcpErr)
	}
	srcPort := localAddr.Port
	util.LogDebug("[NETSTACK-DIAL] bound to local addr=%s srcPort=%d in %v", localAddr.Addr, srcPort, time.Since(bindStart))

	// Register in ModeBTable BEFORE connect (before SYN is sent)
	if modeBTable != nil {
		dstKey := net.JoinHostPort(host, port)
		modeBTable.Register(6, dstKey, srcPort, clientAddr, inbound, mapping)
		util.LogDebug("[NETSTACK-DIAL] registered ModeBTable before SYN: dst=%s srcPort=%d client=%s", dstKey, srcPort, clientAddr)
	}

	// Create wait queue entry for connect completion
	waitEntry, notifyCh := waiter.NewChannelEntry(waiter.WritableEvents)
	wq.EventRegister(&waitEntry)
	defer wq.EventUnregister(&waitEntry)

	select {
	case <-ctx.Done():
		util.LogWarn("[NETSTACK-DIAL] context done before connect")
		ep.Close()
		return nil, ctx.Err()
	default:
	}

	// Now connect (sends SYN)
	util.LogInfo("[NETSTACK-DIAL] connecting to %s:%d (srcPort=%d)", host, portNum, srcPort)
	connectStart := time.Now()
	tcpErr = ep.Connect(remoteAddr)
	if _, ok := tcpErr.(*tcpip.ErrConnectStarted); ok {
		util.LogDebug("[NETSTACK-DIAL] connect in progress, waiting for completion")
		select {
		case <-ctx.Done():
			util.LogWarn("[NETSTACK-DIAL] context timeout while waiting for connect")
			ep.Close()
			return nil, ctx.Err()
		case <-notifyCh:
			util.LogDebug("[NETSTACK-DIAL] connect notification received after %v", time.Since(connectStart))
		}
		tcpErr = ep.LastError()
	}
	connectDuration := time.Since(connectStart)
	if tcpErr != nil {
		util.LogWarn("[NETSTACK-DIAL] connect failed to %s:%d after %v: %v", host, portNum, connectDuration, tcpErr)
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

	totalDuration := time.Since(dialStart)
	util.LogInfo("[NETSTACK-DIAL] succeeded to %s:%d srcPort=%d connect=%v total=%v", host, portNum, srcPort, connectDuration, totalDuration)
	n.applyTCPKeepalive(ep)
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
		util.LogInfo("[TCP-DEBUG] tcp forwarder called local=%s:%d remote=%s:%d",
			net.IP(id.LocalAddress.AsSlice()), id.LocalPort,
			net.IP(id.RemoteAddress.AsSlice()), id.RemotePort)
		var wq waiter.Queue
		util.LogInfo("[TCP-DEBUG] calling CreateEndpoint...")
		ep, err := r.CreateEndpoint(&wq)
		util.LogInfo("[TCP-DEBUG] CreateEndpoint returned, err=%v", err)
		if err != nil {
			util.LogWarn("[TCP-DEBUG] tcp CreateEndpoint fail: %v (local=%s:%d remote=%s:%d)",
				err, net.IP(id.LocalAddress.AsSlice()), id.LocalPort,
				net.IP(id.RemoteAddress.AsSlice()), id.RemotePort)
			r.Complete(true)
			return
		}
		util.LogInfo("[TCP-DEBUG] CreateEndpoint succeeded, calling handleConn async")
		r.Complete(false)

		// Set TCP keepalive on inbound connection
		n.applyTCPKeepalive(ep)

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

	util.LogInfo("[WRITELOOP] started (linkEP=%T, writeLoopDevice=%v)", n.linkEP, n.WriteLoopDevice != nil)

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
			util.LogInfo("[WRITELOOP] ReadContext returned nil, exiting")
			return
		}

		buf := pkt.ToBuffer()
		data := buf.Flatten()

		if len(data) < 20 || (data[0]>>4) != 4 {
			pkt.DecRef()
			continue
		}

		dstIP := net.IP(data[16:20])
		srcIP := net.IP(data[12:16])

		hl := int(data[0]&0x0f) * 4
		pktNum := n.WritePackets.Load()
		if pktNum < 50 || pktNum%1000 == 0 {
			util.LogInfo("[WRITELOOP] pkt#%d: %s:%d -> %s:%d (proto=%d len=%d)",
				pktNum, srcIP, portAt(data, hl), dstIP, portAt(data, hl+2),
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
			// NAT handled by gVisor iptables

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
					if pktNum < 50 || pktNum%1000 == 0 {
						util.LogInfo("[WRITELOOP] wrote pkt#%d to TUN: %s:%d -> %s:%d",
							pktNum, srcIP, portAt(data, hl), net.IP(data[16:20]), portAt(data, hl+2))
					}
					if n.callbacks != nil && n.callbacks.StatsNotify != nil {
						n.callbacks.StatsNotify()
					}
				}
			} else if pktNum < 50 {
				util.LogWarn("[WRITELOOP] WriteLoopDevice is nil, pkt#%d dropped: %s -> %s", pktNum, srcIP, dstIP)
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

// portAt extracts a 16-bit port at the given byte offset (0 if out of range).
func portAt(data []byte, offset int) uint16 {
	if len(data) < offset+2 {
		return 0
	}
	return uint16(data[offset])<<8 | uint16(data[offset+1])
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
