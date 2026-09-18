package mesh

import (
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"phaethon/config"
	"phaethon/util"

	"gvisor.dev/gvisor/pkg/tcpip"
)

const (
	MeshDomainSuffix = "phn"
)

// logTCPPacketMesh logs TCP packet details for debugging
func logTCPPacketMesh(prefix string, data []byte) {
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

func NodeDomain(nodeID string) string {
	return nodeID + "." + MeshDomainSuffix
}

func ParseNodeDomain(domain string) string {
	domain = strings.ToLower(domain)
	suffix := "." + MeshDomainSuffix
	if !strings.HasSuffix(domain, suffix) {
		return ""
	}
	return strings.TrimSuffix(domain, suffix)
}

// TunInterface abstracts the TUN engine for mesh packet injection.
type TunInterface interface {
	InjectMeshPacket(data []byte) error
	WriteMeshPacket(data []byte) error
}

// PeerSender sends data directly to a connected peer.
type PeerSender interface {
	Send(data []byte) error
	SendGossip(data []byte)
	GetNodeID() string
}

// P2PTransport abstracts the P2P layer for mesh packet delivery.
type P2PTransport interface {
	BroadcastMeshGossip(data []byte) error
	SendMeshGossipTo(peerNodeID string, data []byte) error
	SendMeshGossipToAll(data []byte)
	ListMeshPeerIDs() []string
	SetMeshInfo(nodeID, vip string)
}

// MeshPeerInfo describes a connected mesh peer.
type MeshPeerInfo struct {
	NodeID   string    `json:"nodeId"`
	Subnet   string    `json:"subnet"`
	Direct   bool      `json:"direct"`
	LastSeen time.Time `json:"lastSeen"`
}

// PeerWithHop pairs a peer sender with its hop count.
type PeerWithHop struct {
	Peer PeerSender
	Hop  int
}

// MeshRoute represents a route to a network prefix via one or more peers.
// Peers are sorted by hop count (ascending). Selection uses round-robin
// within the lowest-hop group; lastIdx tracks the next index.
type MeshRoute struct {
	Prefix  *net.IPNet
	Peers   []PeerWithHop
	lastIdx int
}

// MeshManager coordinates mesh overlay networking.
type MeshManager struct {
	mu             sync.RWMutex
	nodeID         string
	vip            net.IP
	subnet         *net.IPNet
	subnetStr      string
	domainSuffixes []string
	advertise      []string
	topology       *Topology
	tun            TunInterface
	p2p            P2PTransport
	dataDir        string // for state file persistence

	network         *net.IPNet // overall mesh network (e.g., 100.0.0.0/8)
	subnetPrefixLen int        // per-node subnet prefix length (e.g., 16 for /16)

	// routeTable holds routes and domainTrie, accessed atomically for lock-free reads.
	// Writes create a new routeTable and Store() it atomically.
	routeTable atomic.Value // stores *routeTable

	DNSAllocator func(domain string) (net.IP, error)

	natTable *NATTable
	closeCh  chan struct{}
	eventCh  chan meshEvent

	// DNS hijacker and Fake-IP pool (mesh DNS service)
	dnsHijacker *DNSHijacker
	fakeIPPool  *FakeIPPool

	// IPIP tunnel for static policy routing
	ipipTunnel         *IPIPTunnel
	staticRoutes       []config.MeshStaticRoute
	staticDomainSuffixes []config.MeshStaticDomainSuffix
}

// routeTable is an immutable snapshot of routing state, swapped atomically.
type routeTable struct {
	routes     []MeshRoute // sorted by prefix length (longest first)
	domainTrie *DomainTrie
}

// getRouteTable returns the current route table (lock-free).
func (m *MeshManager) getRouteTable() *routeTable {
	return m.routeTable.Load().(*routeTable)
}

func NewMeshManager(nodeID string, vip net.IP, additionalVIPs []net.IP, subnet *net.IPNet, subnetStr string, domainSuffixes []string, advertise []string, network *net.IPNet, subnetPrefixLen int) *MeshManager {
	m := &MeshManager{
		nodeID:          nodeID,
		vip:             vip.To4(),
		subnet:          subnet,
		subnetStr:       subnetStr,
		domainSuffixes:  domainSuffixes,
		advertise:       advertise,
		topology:        NewTopology(),
		network:         network,
		subnetPrefixLen: subnetPrefixLen,
		closeCh:         make(chan struct{}),
		eventCh:         make(chan meshEvent, 64),
	}
	// Initialize routeTable with empty routes
	m.routeTable.Store(&routeTable{
		routes:     make([]MeshRoute, 0),
		domainTrie: NewDomainTrie(),
	})

	// Create Fake-IP pool from node subnet (skip first 10: .0=network, .1=VIP, .2=hostIP, .3=GIP, .4=EIP, .5-.9=future)
	m.fakeIPPool = NewFakeIPPoolWithSubnet(subnet, 9)

	// Create DNS hijacker (netstack binding deferred to BindNetstack)
	// tunAddr and dnsAddr will be set when binding to netstack
	m.dnsHijacker = NewDNSHijacker(nil, m.fakeIPPool, tcpip.Address{}, tcpip.Address{})
	m.dnsHijacker.SetDomainResolver(m.ResolveDomainSubnet)

	// Create IPIP tunnel and allocate EIP from subnet
	m.ipipTunnel = NewIPIPTunnel()
	if eip := AllocateEIP(subnet); eip != nil {
		m.ipipTunnel.SetLocalEIP(eip)
		util.LogInfo("[MESH] Allocated EIP %s from subnet %s", eip, subnet)
	}

	return m
}

// GetDNSHijacker returns the DNS hijacker for TUN engine binding.
func (m *MeshManager) GetDNSHijacker() *DNSHijacker {
	return m.dnsHijacker
}

// GetFakeIPPool returns the Fake-IP pool for external use.
func (m *MeshManager) GetFakeIPPool() *FakeIPPool {
	return m.fakeIPPool
}

// GetIPIPTunnel returns the IPIP tunnel handler for static policy routing.
func (m *MeshManager) GetIPIPTunnel() *IPIPTunnel {
	return m.ipipTunnel
}

// SetStaticRoutes updates the static IPIP routes from config.
func (m *MeshManager) SetStaticRoutes(staticRoutes []config.MeshStaticRoute, staticDomainSuffixes []config.MeshStaticDomainSuffix) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.staticRoutes = staticRoutes
	m.staticDomainSuffixes = staticDomainSuffixes
	util.LogInfo("[MESH] Updated static routes: %d IP routes, %d domain suffixes", len(staticRoutes), len(staticDomainSuffixes))
}

// CheckStaticRoute checks if a destination IP matches any static IPIP route.
// Returns (egressNodeID, true) if matched, ("", false) otherwise.
func (m *MeshManager) CheckStaticRoute(dstIP net.IP) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.ipipTunnel == nil {
		return "", false
	}
	// Special logging for 8.8.8.0/24 range (our test destination)
	if len(dstIP) >= 4 && dstIP[0] == 8 && dstIP[1] == 8 && dstIP[2] == 8 {
		util.LogDebug("[IPIP] CheckStaticRoute for 8.8.8.x: dst=%s, routes=%d", dstIP, len(m.staticRoutes))
	}
	return m.ipipTunnel.MatchStaticRoute(dstIP, m.staticRoutes)
}

// CheckStaticDomainSuffix checks if a domain matches any static domain suffix route.
// Returns (egressNodeID, true) if matched, ("", false) otherwise.
func (m *MeshManager) CheckStaticDomainSuffix(domain string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.ipipTunnel == nil {
		return "", false
	}
	return m.ipipTunnel.MatchStaticDomainSuffix(domain, m.staticDomainSuffixes)
}

// SetDataDir sets the data directory for state file persistence.
func (m *MeshManager) SetDataDir(dataDir string) {
	m.dataDir = dataDir
}

// checkSubnetConflict checks if our subnet conflicts with any claimed subnet.
// If our nodeId is numerically smaller, we need to re-select.
// Returns true if we re-selected.
func (m *MeshManager) checkSubnetConflict() bool {
	// Collect all claimed subnets from topology
	usedSubnets := make(map[string]bool)
	for _, peer := range m.topology.GetAllPeers() {
		// Peer's own subnet
		if peer.SubnetStr != "" && peer.SubnetStr != m.subnetStr {
			usedSubnets[peer.SubnetStr] = true
		}
		// Peer's learned claims
		for _, cs := range peer.ClaimedSubnets {
			if cs.SubnetStr != m.subnetStr {
				usedSubnets[cs.SubnetStr] = true
			}
		}
	}

	// Check if our subnet is claimed by someone else
	for _, peer := range m.topology.GetAllPeers() {
		// Check peer's own subnet
		if peer.SubnetStr == m.subnetStr && peer.NodeID() != m.nodeID {
			// Conflict! Compare nodeIds — larger wins
			if compareNodeIDs(m.nodeID, peer.NodeID()) < 0 {
				// Our nodeId is smaller, we lose — re-select
				return m.reselectSubnet(usedSubnets)
			}
			// Otherwise, they will re-select
		}
		// Check peer's learned claims
		for _, cs := range peer.ClaimedSubnets {
			if cs.SubnetStr == m.subnetStr && cs.NodeID != m.nodeID {
				if compareNodeIDs(m.nodeID, cs.NodeID) < 0 {
					return m.reselectSubnet(usedSubnets)
				}
			}
		}
	}
	return false
}

// reselectSubnet picks a new subnet that doesn't conflict.
func (m *MeshManager) reselectSubnet(usedSubnets map[string]bool) bool {
	// Mark our current subnet as used so we don't pick it again
	usedSubnets[m.subnetStr] = true

	newSubnetStr, err := AllocateSubnet(m.network, m.subnetPrefixLen, usedSubnets)
	if err != nil {
		util.LogError("[MESH] subnet conflict: failed to allocate new subnet: %v", err)
		return false
	}

	_, newSubnet, err := net.ParseCIDR(newSubnetStr)
	if err != nil {
		util.LogError("[MESH] subnet conflict: invalid new subnet %s: %v", newSubnetStr, err)
		return false
	}

	m.mu.Lock()
	m.subnet = newSubnet
	m.subnetStr = newSubnetStr
	m.vip = DeriveVIPFromSubnet(newSubnet)
	m.mu.Unlock()

	util.LogInfo("[MESH] subnet conflict: re-selected subnet to %s (vip=%s)", newSubnetStr, m.vip)

	// Update P2P layer with new VIP
	if m.p2p != nil {
		m.p2p.SetMeshInfo(m.nodeID, m.vip.String())
	}

	// Save state
	if m.dataDir != "" {
		state := &MeshState{
			NodeID: m.nodeID,
			Subnet: newSubnetStr,
		}
		if err := SaveState(m.dataDir, state); err != nil {
			util.LogError("[MESH] failed to save state after subnet re-selection: %v", err)
		}
	}

	return true
}

// compareNodeIDs compares two nodeID strings.
// Returns -1 if a < b, 0 if a == b, 1 if a > b.
// Conflict resolution: larger nodeID wins. Non-numeric strings (e.g. "vm")
// are compared lexicographically and naturally beat numeric strings in ASCII order,
// giving manually named nodes priority over auto-generated ones.
func compareNodeIDs(a, b string) int {
	aNum, aErr := strconv.ParseUint(a, 10, 64)
	bNum, bErr := strconv.ParseUint(b, 10, 64)
	if aErr != nil || bErr != nil {
		// Fall back to string comparison if not numeric
		if a < b {
			return -1
		} else if a > b {
			return 1
		}
		return 0
	}
	if aNum < bNum {
		return -1
	} else if aNum > bNum {
		return 1
	}
	return 0
}

func (m *MeshManager) GetVIP() net.IP {
	return m.vip
}

func (m *MeshManager) GetNodeID() string {
	return m.nodeID
}

func (m *MeshManager) GetAllVIPs() []net.IP {
	return []net.IP{m.vip}
}

func (m *MeshManager) GetSubnet() string {
	return m.subnetStr
}

func (m *MeshManager) EnableNAT() {
	m.natTable = NewNATTable(m.vip)
	util.LogInfo("[MESH] NAT enabled (vip=%s)", m.vip)
}

func (m *MeshManager) GetNATStats() int {
	if m.natTable == nil {
		return 0
	}
	return m.natTable.Stats()
}

func (m *MeshManager) GetNATTable() *NATTable {
	return m.natTable
}

func (m *MeshManager) GetDomainSuffixes() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]string, len(m.domainSuffixes))
	copy(result, m.domainSuffixes)
	return result
}

// UpdateConfig hot-swaps domainSuffixes and advertise without restarting.
func (m *MeshManager) UpdateConfig(domainSuffixes, advertise []string) {
	m.mu.Lock()
	m.domainSuffixes = domainSuffixes
	m.advertise = advertise
	m.mu.Unlock()
	select {
	case m.eventCh <- meshEvent{kind: meshEventConfigUpdate}:
	default:
		util.LogDebug("[MESH] eventCh full, dropping config update event")
	}
}

// TriggerGossip sends an immediate gossip broadcast.
func (m *MeshManager) TriggerGossip() {
	select {
	case m.eventCh <- meshEvent{kind: meshEventTick}:
	default:
		util.LogDebug("[MESH] eventCh full, dropping trigger gossip event")
	}
}

func (m *MeshManager) TopologyRef() *Topology {
	return m.topology
}

func (m *MeshManager) isLocalVIP(ip net.IP) bool {
	if m.vip == nil {
		return false
	}
	return ip.Equal(m.vip)
}

// getVIPForNode returns the VIP for a given node ID from the topology.
func (m *MeshManager) getVIPForNode(nodeID string) net.IP {
	// Check if it's our own node
	if nodeID == m.nodeID {
		return m.vip
	}
	// Look up in topology
	for _, peer := range m.topology.GetAllPeers() {
		if peer.NodeID() == nodeID && peer.Subnet != nil {
			// VIP is subnet + 1 (first usable IP)
			baseIP := peer.Subnet.IP.To4()
			if baseIP != nil {
				return net.IP{baseIP[0], baseIP[1], baseIP[2], baseIP[3] + 1}
			}
		}
	}
	return nil
}

// getSubnetForNode returns the subnet for a given node ID from the topology.
func (m *MeshManager) getSubnetForNode(nodeID string) *net.IPNet {
	// Check if it's our own node
	if nodeID == m.nodeID {
		return m.subnet
	}
	// Look up in topology
	for _, peer := range m.topology.GetAllPeers() {
		if peer.NodeID() == nodeID && peer.Subnet != nil {
			return peer.Subnet
		}
	}
	return nil
}

// getEIPForNode calculates and returns the EIP for a given node ID.
// EIP is deterministically calculated from the node's subnet (last usable IP).
func (m *MeshManager) getEIPForNode(nodeID string) net.IP {
	subnet := m.getSubnetForNode(nodeID)
	if subnet == nil {
		return nil
	}
	return CalculateEIP(subnet)
}

// getHostIP returns the hostIP (.2) address for this node's subnet.
// hostIP is the TUN interface address on the OS side.
func (m *MeshManager) getHostIP() net.IP {
	if m.subnet == nil {
		return nil
	}
	baseIP := m.subnet.IP.To4()
	if baseIP == nil {
		return nil
	}
	return net.IP{baseIP[0], baseIP[1], baseIP[2], baseIP[3] + 2}
}

// getGIP returns the GIP (.3) address for this node's subnet.
// GIP is the DNS service address, delivered to gVisor netstack.
func (m *MeshManager) getGIP() net.IP {
	if m.subnet == nil {
		return nil
	}
	baseIP := m.subnet.IP.To4()
	if baseIP == nil {
		return nil
	}
	return net.IP{baseIP[0], baseIP[1], baseIP[2], baseIP[3] + 3}
}

// isLocalHostIP checks if the IP is the local hostIP (.2) address.
func (m *MeshManager) isLocalHostIP(ip net.IP) bool {
	hostIP := m.getHostIP()
	return hostIP != nil && ip.Equal(hostIP)
}

// isLocalGIP checks if the IP is the local GIP (.3) address.
func (m *MeshManager) isLocalGIP(ip net.IP) bool {
	gip := m.getGIP()
	return gip != nil && ip.Equal(gip)
}

// isLocalNetstackAddr checks if the IP is a local netstack address (GIP .3 or hostIP .2).
// These addresses are handled by the netstack internally via InjectInbound, not by mesh routing.
func (m *MeshManager) isLocalNetstackAddr(ip net.IP) bool {
	return m.isLocalHostIP(ip) || m.isLocalGIP(ip)
}

func (m *MeshManager) SetTun(tun TunInterface) {
	m.tun = tun
}

var GlobalMeshManager *MeshManager

func (m *MeshManager) Start(tun TunInterface, p2p P2PTransport) {
	m.tun = tun
	m.p2p = p2p
	// Set mesh info on P2P layer so hello messages include correct meshNodeId
	p2p.SetMeshInfo(m.nodeID, m.vip.String())
	m.recomputeRoutes()
	go m.gossipLoop()
	util.LogInfo("[MESH] started: nodeID=%s vip=%s subnet=%s subnetStr=%s", m.nodeID, m.vip, m.subnet, m.subnetStr)
}

func (m *MeshManager) Stop() {
	close(m.closeCh)
}

// ResolveDomainSubnet looks up a domain in the domain trie and returns the
// Fake-IP subnet of the remote node that owns the matching suffix.
// Returns nil if the domain is local (this node owns the suffix) or no match.
func (m *MeshManager) ResolveDomainSubnet(domain string) *net.IPNet {
	// Check static domain suffixes first
	m.mu.RLock()
	staticSuffixes := m.staticDomainSuffixes
	m.mu.RUnlock()
	
	for _, suffix := range staticSuffixes {
		s := strings.ToLower(suffix.Suffix)
		d := strings.ToLower(domain)
		if d == s || strings.HasSuffix(d, "."+s) {
			// Matched static domain suffix, get target node's subnet
			targetNodeID := suffix.Via
			util.LogDebug("[MESH] ResolveDomainSubnet(%s): matched static suffix %s via %s", domain, suffix.Suffix, targetNodeID)
			
			// Get target node's subnet from topology
			for _, peer := range m.topology.GetAllPeers() {
				if peer.NodeID() == targetNodeID && peer.Subnet != nil {
					util.LogDebug("[MESH] ResolveDomainSubnet(%s): found node %s subnet %s", domain, targetNodeID, peer.Subnet)
					return peer.Subnet
				}
			}
			// Node not found in topology yet, return nil
			util.LogWarn("[MESH] ResolveDomainSubnet(%s): node %s not found in topology", domain, targetNodeID)
			return nil
		}
	}
	
	// Fall back to advertised domain trie
	rt := m.getRouteTable()
	trie := rt.domainTrie
	if trie == nil {
		return nil
	}
	peers, subnet, suffixLen := trie.Lookup(domain)
	var peerIDs []string
	for _, p := range peers {
		peerIDs = append(peerIDs, p.Peer.GetNodeID())
	}
	var subnetStr string
	if subnet != nil {
		subnetStr = subnet.String()
	}
	util.LogDebug("[MESH] ResolveDomainSubnet(%s): suffixLen=%d peers=%v subnet=%s", domain, suffixLen, peerIDs, subnetStr)
	if suffixLen == 0 || len(peers) == 0 {
		return nil // no match or local entry
	}
	return subnet
}

// RegisterPeer is called when a P2P peer with mesh capability connects.
func (m *MeshManager) RegisterPeer(sender PeerSender) {
	select {
	case m.eventCh <- meshEvent{kind: meshEventRegister, sender: sender}:
	default:
		util.LogDebug("[MESH] eventCh full, dropping peer register event for %s", sender.GetNodeID())
	}
}

// UnregisterPeer is called when a P2P peer disconnects.
func (m *MeshManager) UnregisterPeer(sender PeerSender) {
	select {
	case m.eventCh <- meshEvent{kind: meshEventUnregister, sender: sender}:
	default:
		util.LogDebug("[MESH] eventCh full, dropping peer unregister event for %s", sender.GetNodeID())
	}
}

// HandleOutboundPacket is the TUN readLoop interceptor.
// Returns true if the packet was handled.
func (m *MeshManager) HandleOutboundPacket(dstIP net.IP, data []byte) bool {
	// Very visible log for 8.8.8.x to debug IPIP
	if len(dstIP) >= 4 && dstIP[0] == 8 && dstIP[1] == 8 && dstIP[2] == 8 {
		util.LogDebug("[IPIP] HandleOutboundPacket called for 8.8.8.x: dst=%s len=%d", dstIP, len(data))
	}

	// Debug: log all packets to mesh network
	if isMeshAddress(dstIP) {
		proto := "unknown"
		if len(data) >= 20 && data[0]>>4 == 4 {
			if data[9] == 6 {
				proto = "TCP"
			} else if data[9] == 17 {
				proto = "UDP"
			}
		}
		util.LogDebug("[MESH] HandleOutboundPacket: dst=%s proto=%s len=%d", dstIP, proto, len(data))
	} else if len(data) >= 20 && data[0]>>4 == 4 {
		// Log non-mesh IPv4 packets for debugging static routes
		util.LogDebug("[MESH] HandleOutboundPacket non-mesh: dst=%s len=%d", dstIP, len(data))
	}

	// Exclude local netstack addresses (GIP .3, hostIP .2) from mesh interception.
	// These packets must reach InjectInbound so the netstack's DNS hijacker can process them.
	if m.isLocalNetstackAddr(dstIP) {
		return false
	}

	if m.isLocalVIP(dstIP) {
		if m.tun != nil {
			if err := m.tun.WriteMeshPacket(data); err != nil {
				util.LogWarn("[MESH] write local packet to TUN failed: %v", err)
			}
		}
		return true
	}

	// Check static IPIP routes before normal mesh routing
	if egressNodeID, matched := m.CheckStaticRoute(dstIP); matched {
		util.LogInfo("[IPIP] Static route matched: dst=%s via=%s", dstIP, egressNodeID)
		util.LogInfo("[IPIP] Attempting encapsulation via %s", egressNodeID)
		
		// Get egress node's EIP (calculated from its advertised subnet)
		egressEIP := m.getEIPForNode(egressNodeID)
		if egressEIP == nil {
			util.LogWarn("[IPIP] No EIP for egress node %s (subnet not in topology), dropping packet", egressNodeID)
			return true
		}
		
		// Get local EIP
		localEIP := m.ipipTunnel.GetLocalEIP()
		if localEIP == nil {
			util.LogWarn("[IPIP] No local EIP, dropping packet")
			return true
		}
		
		// Encapsulate the packet
		encapsulated, err := m.ipipTunnel.Encapsulate(localEIP, egressEIP, data)
		if err != nil {
			util.LogWarn("[IPIP] Encapsulation failed: %v", err)
			return true
		}
		
		util.LogDebug("[IPIP] Encapsulated packet: outer src=%s dst=%s inner len=%d total len=%d",
			localEIP, egressEIP, len(data), len(encapsulated))
		
		// Find route to egress node's VIP and send encapsulated packet
		egressVIP := m.getVIPForNode(egressNodeID)
		if egressVIP == nil {
			util.LogWarn("[IPIP] No VIP for egress node %s", egressNodeID)
			return true
		}
		
		// Use normal mesh routing to send encapsulated packet to egress node
		route := m.findRoute(egressVIP)
		if route != nil && len(route.Peers) > 0 {
			selectedPeer := route.Peers[0].Peer
			if err := selectedPeer.Send(encapsulated); err != nil {
				util.LogWarn("[IPIP] Send to %s failed: %v", egressNodeID, err)
			} else {
				util.LogDebug("[IPIP] Sent encapsulated packet to %s OK", egressNodeID)
			}
		} else {
			util.LogWarn("[IPIP] No route to egress node %s (VIP=%s)", egressNodeID, egressVIP)
		}
		return true
	}

	// Exclude mesh subnet (Fake-IPs) from mesh interception.
	// Fake-IPs are allocated from the mesh subnet but are not actual VIPs.
	// They must reach InjectInbound so the gVisor TCP forwarder can handle them
	// and look up the original domain via fakeIP.LookupDomain.
	//
	// IMPORTANT: Check findRoute FIRST before local subnet check.
	// Remote Fake-IPs (e.g., 100.64.0.x on VM with subnet 100.64.1.0/24) should be
	// routed via the peer that owns that subnet, not passed to local netstack.
	// Round-robin across peers with the lowest hop count.
	route := m.findRoute(dstIP)
	if route != nil && len(route.Peers) > 0 {
		// Find lowest hop count and count peers at that hop.
		minHop := route.Peers[0].Hop
		count := 0
		for _, p := range route.Peers {
			if p.Hop == minHop {
				count++
			} else {
				break // sorted, so different hop means we're done
			}
		}

		// Round-robin selection within the lowest-hop group.
		idx := route.lastIdx % count
		selectedPeer := route.Peers[idx].Peer
		route.lastIdx++

		// Debug log for VIP-like destinations
		if len(dstIP) >= 4 && dstIP[3] == 1 {
			util.LogDebug("[MESH] Sending to %s: selected peer=%s (hop=%d, idx=%d/%d)",
				dstIP, selectedPeer.GetNodeID(), minHop, idx, count)
		}

		// Remote peer owns this IP — send via mesh
		pkt := make([]byte, len(data))
		copy(pkt, data)

		if isMeshAddress(dstIP) {
			util.LogDebug("[MESH] outbound %s: sending %d bytes via peer %s (hop=%d, idx=%d/%d)",
				dstIP, len(pkt), selectedPeer.GetNodeID(), minHop, idx, count)
			if len(pkt) >= 20 && pkt[9] == 6 {
				logTCPPacketMesh("[TCP] outbound:", pkt)
			}
		}

		if err := selectedPeer.Send(pkt); err != nil {
			util.LogWarn("[MESH] send to %s failed: %v", selectedPeer.GetNodeID(), err)
		} else {
			// Debug log for successful sends to VIP-like destinations
			if len(dstIP) >= 4 && dstIP[3] == 1 {
				util.LogDebug("[MESH] Successfully sent %d bytes to %s via %s",
					len(pkt), dstIP, selectedPeer.GetNodeID())
			} else if len(pkt) >= 20 && pkt[0]>>4 == 4 {
				dst := net.IP(pkt[16:20])
				if isMeshAddress(dst) {
					util.LogDebug("[MESH] sent %d bytes to %s via peer %s OK", len(pkt), dst, selectedPeer.GetNodeID())
				}
			}
		}
		return true
	}

	// Local route (peers empty) or no route — pass through to netstack
	if isMeshAddress(dstIP) {
		if route != nil {
			util.LogDebug("[MESH] outbound %s: local route %s, passing through", dstIP, route.Prefix)
		} else {
			util.LogDebug("[MESH] outbound %s: no route, passing through", dstIP)
		}
	}
	return false
}

// HandleMeshFrame processes a raw IP packet received from a peer.
func (m *MeshManager) HandleMeshFrame(fromNodeID string, frame []byte) {
	util.LogDebug("[MESH] HandleMeshFrame called from %s: %d bytes", fromNodeID, len(frame))
	if len(frame) < 20 || frame[0]>>4 != 4 {
		util.LogWarn("[MESH] bad packet from %s: %d bytes", fromNodeID, len(frame))
		return
	}

	// Check if this is an IPIP packet (protocol 4)
	if frame[9] == 4 {
		util.LogInfo("[IPIP] Received IPIP packet from %s, decapsulating", fromNodeID)
		innerPacket, err := Decapsulate(frame)
		if err != nil {
			util.LogWarn("[IPIP] Decapsulation failed: %v", err)
			return
		}
		util.LogDebug("[IPIP] Decapsulated packet: inner len=%d", len(innerPacket))
		// Recursively process the inner packet
		m.HandleMeshFrame(fromNodeID, innerPacket)
		return
	}

	dstIP := extractDstIP(frame)
	srcIP := net.IP(frame[12:16])
	if dstIP == nil {
		util.LogWarn("[MESH] bad packet from %s: cannot extract dst IP", fromNodeID)
		return
	}
	if isMeshAddress(dstIP) && len(frame) >= 20 && frame[9] == 6 {
		dstPort := uint16(frame[22])<<8 | uint16(frame[23])
		util.LogDebug("[MESH] recv TCP from %s: src=%s dst=%s:%d len=%d", fromNodeID, srcIP, dstIP, dstPort, len(frame))
	}
	util.LogDebug("[MESH] HandleMeshFrame from %s: src=%s dst=%s proto=%d len=%d",
		fromNodeID, srcIP, dstIP, frame[9], len(frame))

	if isMeshAddress(dstIP) {
		util.LogDebug("[MESH] recv frame from %s: dst=%s TTL=%d len=%d", fromNodeID, dstIP, frame[8], len(frame))
		if len(frame) >= 20 && frame[9] == 6 {
			logTCPPacketMesh("[TCP] recv:", frame)
		}
	}

	isVIP := m.isLocalVIP(dstIP)
	util.LogDebug("[MESH] checking VIP: dst=%s isVIP=%v vip=%v", dstIP, isVIP, m.vip)

	// .1 (VIP): NAT reverse + WriteMeshPacket to OS
	// VIP is only for locally-originated connections (via NAT).
	// If NAT reverse fails, drop the packet - it's not a valid response.
	if isVIP {
		util.LogDebug("[MESH] VIP path entered for packet from %s", fromNodeID)
		if m.natTable == nil {
			util.LogWarn("[MESH] VIP packet from %s dropped: natTable is nil", fromNodeID)
			return
		}
		pkt := make([]byte, len(frame))
		copy(pkt, frame)

		natPkt := m.natTable.TranslateInbound(pkt)
		if natPkt != nil {
			if m.tun != nil {
				if err := m.tun.WriteMeshPacket(natPkt); err != nil {
					util.LogWarn("[MESH] write VIP packet to TUN failed: %v", err)
				}
			}
			return
		}
		util.LogDebug("[MESH] VIP packet from %s dropped: NAT reverse failed", fromNodeID)
		return
	}

	// .2 (hostIP): src rewrite to local GIP + WriteMeshPacket to OS
	// hostIP traffic never went through NAT, so only src rewrite is needed.
	if m.isLocalHostIP(dstIP) {
		pkt := make([]byte, len(frame))
		copy(pkt, frame)
		localGIP := m.getGIP()
		if localGIP != nil {
			if m.natTable != nil {
				srcPkt := m.natTable.RewriteSrcIP(pkt, localGIP)
				if srcPkt != nil {
					pkt = srcPkt
				}
			} else {
				rewritePkt := rewriteSrcIPInPacket(pkt, localGIP)
				if rewritePkt != nil {
					pkt = rewritePkt
				}
			}
		}
		if m.tun != nil {
			if err := m.tun.WriteMeshPacket(pkt); err != nil {
				util.LogWarn("[MESH] write hostIP packet to TUN failed: %v", err)
			}
		}
		return
	}

	// .3 (GIP): InjectMeshPacket to netstack (DNS hijacker)
	if m.isLocalGIP(dstIP) {
		pkt := make([]byte, len(frame))
		copy(pkt, frame)
		if m.tun != nil {
			if err := m.tun.InjectMeshPacket(pkt); err != nil {
				util.LogWarn("[MESH] inject GIP packet to netstack failed: %v", err)
			}
		}
		return
	}

	if frame[8] <= 1 {
		if isMeshAddress(dstIP) {
			util.LogDebug("[MESH] recv frame from %s: dst=%s TTL=%d dropped (TTL<=1)", fromNodeID, dstIP, frame[8])
		}
		return
	}

	if isMeshAddress(dstIP) && len(frame) >= 20 && frame[9] == 6 {
		util.LogDebug("[MESH] pre-findRoute: from=%s dst=%s TTL=%d", fromNodeID, dstIP, frame[8])
	}

	route := m.findRoute(dstIP)
	if route == nil || len(route.Peers) == 0 {
		// No mesh route — we're the gateway for this destination.
		// Inject into local netstack so it goes out via proxy/direct.
		if isMeshAddress(dstIP) {
			proto := "unknown"
			if len(frame) >= 20 {
				switch frame[9] {
				case 6:
					proto = "TCP"
				case 17:
					proto = "UDP"
				}
			}
			util.LogDebug("[MESH] recv frame from %s: dst=%s proto=%s injecting to local netstack", fromNodeID, dstIP, proto)
		}
		pkt := make([]byte, len(frame))
		copy(pkt, frame)
		if m.tun != nil {
			if err := m.tun.InjectMeshPacket(pkt); err != nil {
				util.LogWarn("[MESH] inject to local netstack failed: %v", err)
			}
		}
		return
	}

	// Round-robin selection within the lowest-hop group.
	minHop := route.Peers[0].Hop
	count := 0
	for _, p := range route.Peers {
		if p.Hop == minHop {
			count++
		} else {
			break
		}
	}
	idx := route.lastIdx % count
	selectedPeer := route.Peers[idx].Peer
	route.lastIdx++

	if isMeshAddress(dstIP) {
		util.LogDebug("[MESH] forwarding from %s: dst=%s to %s (hop=%d, idx=%d/%d)", fromNodeID, dstIP, selectedPeer.GetNodeID(), minHop, idx, count)
	}
	pkt := make([]byte, len(frame))
	copy(pkt, frame)
	decrementIPTTL(pkt)
	if err := selectedPeer.Send(pkt); err != nil {
		util.LogWarn("[MESH] forward to %s failed: %v", selectedPeer.GetNodeID(), err)
	}
}

// HandleTopologyGossip processes a gossip announcement from a peer.
func (m *MeshManager) HandleTopologyGossip(sender PeerSender, data []byte) {
	var info GossipInfo
	if err := json.Unmarshal(data, &info); err != nil {
		util.LogDebug("[MESH] bad gossip from %s: %v", sender.GetNodeID(), err)
		return
	}
	if info.NodeID == m.nodeID {
		return
	}
	select {
	case m.eventCh <- meshEvent{kind: meshEventGossip, sender: sender, data: data}:
	default:
		util.LogDebug("[MESH] eventCh full, dropping gossip from %s", sender.GetNodeID())
	}
}

func (m *MeshManager) GetStatus() map[string]interface{} {
	rt := m.getRouteTable()
	routeCount := len(rt.routes)

	m.mu.RLock()
	domainSuffixes := make([]string, len(m.domainSuffixes))
	copy(domainSuffixes, m.domainSuffixes)
	advertise := make([]string, len(m.advertise))
	copy(advertise, m.advertise)
	m.mu.RUnlock()

	return map[string]interface{}{
		"enabled":        true,
		"nodeId":         m.nodeID,
		"vip":            m.vip.String(),
		"subnet":         m.subnetStr,
		"domainSuffixes": domainSuffixes,
		"advertise":      advertise,
		"routeCount":     routeCount,
	}
}

func (m *MeshManager) GetTopology() map[string]interface{} {
	peers := m.topology.GetAllPeers()
	peerList := make([]map[string]interface{}, 0, len(peers))
	for _, p := range peers {
		entry := map[string]interface{}{
			"nodeId":   p.NodeID(),
			"subnet":   p.SubnetStr,
			"lastSeen": p.LastSeen,
		}
		if len(p.DomainSuffixes) > 0 {
			entry["domainSuffixes"] = p.DomainSuffixes
		}
		if len(p.Routes) > 0 {
			routeEntries := make([]map[string]interface{}, 0, len(p.Routes))
			for _, r := range p.Routes {
				routeEntries = append(routeEntries, map[string]interface{}{
					"prefix": r.PrefixStr,
					"hop":    r.Hop,
				})
			}
			entry["routes"] = routeEntries
		}
		if len(p.ClaimedSubnets) > 0 {
			claimEntries := make([]map[string]interface{}, 0, len(p.ClaimedSubnets))
			for _, cs := range p.ClaimedSubnets {
				claimEntries = append(claimEntries, map[string]interface{}{
					"subnet": cs.SubnetStr,
					"nodeId": cs.NodeID,
					"hop":    cs.Hop,
				})
			}
			entry["claimedSubnets"] = claimEntries
		}
		peerList = append(peerList, entry)
	}
	return map[string]interface{}{"peers": peerList}
}

// FullTopologyNode represents a node in the full topology.
type FullTopologyNode struct {
	NodeID string `json:"nodeId"`
	VIP    string `json:"vip"`
	Subnet string `json:"subnet"`
}

// FullTopologyEdge represents an edge in the full topology.
type FullTopologyEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// GetFullTopology returns the complete network topology including all known nodes and edges.
func (m *MeshManager) GetFullTopology() map[string]interface{} {
	peers := m.topology.GetAllPeers()

	// Build node list: local node + all peers
	nodes := make([]FullTopologyNode, 0, len(peers)+1)

	// Add local node
	localVIP := ""
	if m.subnet != nil {
		vip := DeriveVIPFromSubnet(m.subnet)
		if vip != nil {
			localVIP = vip.String()
		}
	}
	nodes = append(nodes, FullTopologyNode{
		NodeID: m.nodeID,
		VIP:    localVIP,
		Subnet: m.subnetStr,
	})

	// Add peer nodes
	nodeSet := make(map[string]bool)
	nodeSet[m.nodeID] = true
	for _, p := range peers {
		peerID := p.NodeID()
		if nodeSet[peerID] {
			continue
		}
		nodeSet[peerID] = true
		peerVIP := ""
		if p.Subnet != nil {
			vip := DeriveVIPFromSubnet(p.Subnet)
			if vip != nil {
				peerVIP = vip.String()
			}
		}
		nodes = append(nodes, FullTopologyNode{
			NodeID: peerID,
			VIP:    peerVIP,
			Subnet: p.SubnetStr,
		})
	}

	// Build edge list: local peer connections + learned edges
	edgeSet := make(map[string]FullTopologyEdge)

	// Local direct peer connections
	for _, p := range peers {
		peerID := p.NodeID()
		key := edgeKey(m.nodeID, peerID)
		edgeSet[key] = FullTopologyEdge{From: m.nodeID, To: peerID}
	}

	// Learned edges from gossip
	for _, p := range peers {
		for _, e := range p.TopologyEdges {
			key := edgeKey(e.NodeID, e.Neighbor)
			if _, exists := edgeSet[key]; !exists {
				edgeSet[key] = FullTopologyEdge{From: e.NodeID, To: e.Neighbor}
			}
		}
	}

	// Ensure all nodes referenced in edges are included in the node list
	// This handles non-adjacent nodes learned via gossip
	for _, e := range edgeSet {
		for _, nodeID := range []string{e.From, e.To} {
			if !nodeSet[nodeID] {
				nodeSet[nodeID] = true
				// Try to find subnet info from peers' claimed subnets
				var vip, subnet string
				for _, p := range peers {
					for _, cs := range p.ClaimedSubnets {
						if cs.NodeID == nodeID {
							subnet = cs.SubnetStr
							if cs.Subnet != nil {
								if v := DeriveVIPFromSubnet(cs.Subnet); v != nil {
									vip = v.String()
								}
							}
							break
						}
					}
					if subnet != "" {
						break
					}
				}
				nodes = append(nodes, FullTopologyNode{
					NodeID: nodeID,
					VIP:    vip,
					Subnet: subnet,
				})
			}
		}
	}

	edges := make([]FullTopologyEdge, 0, len(edgeSet))
	for _, e := range edgeSet {
		edges = append(edges, e)
	}

	return map[string]interface{}{
		"nodes": nodes,
		"edges": edges,
	}
}

func (m *MeshManager) GetRoutes() map[string]interface{} {
	rt := m.getRouteTable()

	routeList := make([]map[string]interface{}, 0, len(rt.routes))
	for _, r := range rt.routes {
		var peerInfos []map[string]interface{}
		for _, p := range r.Peers {
			peerInfos = append(peerInfos, map[string]interface{}{
				"nodeID": p.Peer.GetNodeID(),
				"hop":    p.Hop,
			})
		}
		routeList = append(routeList, map[string]interface{}{
			"prefix": r.Prefix.String(),
			"via":    peerInfos,
		})
	}
	return map[string]interface{}{"routes": routeList}
}

func (m *MeshManager) GetPeers() []MeshPeerInfo {
	if m.p2p == nil {
		return nil
	}
	allPeers := m.topology.GetAllPeers()

	// Deduplicate by nodeID (multiple connections to same node)
	seen := make(map[string]bool)
	result := make([]MeshPeerInfo, 0)
	for _, p := range allPeers {
		if p.Sender == nil {
			continue
		}
		nodeID := p.NodeID()
		if nodeID == "" || seen[nodeID] {
			continue
		}
		seen[nodeID] = true
		result = append(result, MeshPeerInfo{
			NodeID:   nodeID,
			Direct:   true,
			Subnet:   p.SubnetStr,
			LastSeen: p.LastSeen,
		})
	}
	return result
}

func (m *MeshManager) ResolveMeshDomain(domain string) net.IP {
	nodeID := ParseNodeDomain(domain)
	if nodeID == "" {
		return nil
	}
	for _, peer := range m.topology.GetAllPeers() {
		if peer.NodeID() == nodeID && peer.Subnet != nil {
			vip := DeriveVIPFromSubnet(peer.Subnet)
			if vip != nil {
				util.LogDebug("[MESH] DNS resolve: %s -> %s", domain, vip)
			}
			return vip
		}
	}
	return nil
}

func (m *MeshManager) recomputeRoutes() {
	m.mu.RLock()
	advertise := make([]string, len(m.advertise))
	copy(advertise, m.advertise)
	domainSuffixes := make([]string, len(m.domainSuffixes))
	copy(domainSuffixes, m.domainSuffixes)
	m.mu.RUnlock()

	peers := m.topology.GetAllPeers()

	// Collect all peers per prefix (not just the best one).
	// ownPrefixes tracks prefixes owned by this node (no forwarding needed).
	type peerEntry struct {
		sender PeerSender
		hop    int
		prefix *net.IPNet
	}
	allEntries := make(map[string][]peerEntry)
	ownPrefixes := make(map[string]bool)

	// Own subnet (Hop=0) — this node owns it, no forwarding.
	_, ownSubnet, _ := net.ParseCIDR(m.subnetStr)
	if ownSubnet != nil {
		ownPrefixes[m.subnetStr] = true
	}

	// Own advertise routes — this node owns them, no forwarding.
	for _, r := range advertise {
		_, _, err := net.ParseCIDR(r)
		if err != nil {
			continue
		}
		ownPrefixes[r] = true
	}

	// Peer routes (non-mesh, with hop counts)
	for _, peer := range peers {
		if peer.Sender == nil {
			continue
		}
		for _, r := range peer.Routes {
			allEntries[r.PrefixStr] = append(allEntries[r.PrefixStr], peerEntry{peer.Sender, r.Hop, r.Prefix})
		}
	}

	// Peer claimed subnets (mesh routing)
	for _, peer := range peers {
		if peer.Sender == nil {
			continue
		}
		if peer.Subnet != nil {
			key := peer.Subnet.String()
			allEntries[key] = append(allEntries[key], peerEntry{peer.Sender, 1, peer.Subnet})
		}
		for _, cs := range peer.ClaimedSubnets {
			allEntries[cs.SubnetStr] = append(allEntries[cs.SubnetStr], peerEntry{peer.Sender, cs.Hop, cs.Subnet})
		}
	}

	// Build MeshRoute slice: exclude own prefixes from peer routes, sort peers by hop.
	routes := make([]MeshRoute, 0, len(allEntries)+1+len(advertise))
	for prefixStr, entries := range allEntries {
		if ownPrefixes[prefixStr] {
			continue
		}
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].hop < entries[j].hop
		})
		peerList := make([]PeerWithHop, len(entries))
		for i, e := range entries {
			peerList[i] = PeerWithHop{Peer: e.sender, Hop: e.hop}
		}
		routes = append(routes, MeshRoute{
			Prefix:  entries[0].prefix,
			Peers:   peerList,
			lastIdx: 0,
		})
	}

	// Add own subnet as local route (peers empty = local).
	if ownSubnet != nil {
		routes = append(routes, MeshRoute{
			Prefix:  ownSubnet,
			Peers:   nil,
			lastIdx: 0,
		})
	}

	// Add advertise routes as local routes.
	for _, r := range advertise {
		_, ipNet, err := net.ParseCIDR(r)
		if err != nil {
			continue
		}
		routes = append(routes, MeshRoute{
			Prefix:  ipNet,
			Peers:   nil,
			lastIdx: 0,
		})
	}

	// Sort routes by prefix length (longest first) for longest-match lookup.
	sort.Slice(routes, func(i, j int) bool {
		lenI, _ := routes[i].Prefix.Mask.Size()
		lenJ, _ := routes[j].Prefix.Mask.Size()
		return lenI > lenJ
	})

	// Build global domain trie
	trie := NewDomainTrie()
	// Own domain suffixes (Hop=0, NextHop=nil)
	ownSuffixSet := make(map[string]bool, len(domainSuffixes))
	for _, s := range domainSuffixes {
		trie.Insert(s, nil, nil, 0)
		ownSuffixSet[strings.ToLower(strings.TrimPrefix(s, "."))] = true
	}
	// Peer domain suffixes — skip if we own the same suffix
	for _, peer := range peers {
		if peer.Sender == nil {
			continue
		}
		for _, entry := range peer.DomainSuffixes {
			normalized := strings.ToLower(strings.TrimPrefix(entry.Suffix, "."))
			if ownSuffixSet[normalized] {
				continue
			}
			trie.Insert(entry.Suffix, peer.Sender, entry.Subnet, entry.Hop)
		}
	}

	// Auto-generate nodeID.phn entries from claimed subnets.
	// Each known node gets a "nodeID.phn" entry in the trie.
	type nodeClaim struct {
		sender PeerSender
		subnet *net.IPNet
		hop    int
	}
	bestNodes := make(map[string]nodeClaim)
	bestNodes[m.nodeID] = nodeClaim{nil, ownSubnet, 0}
	for _, peer := range peers {
		if peer.Sender == nil {
			continue
		}
		nid := peer.NodeID()
		if nid == "" || nid == m.nodeID {
			continue
		}
		if peer.Subnet != nil {
			if existing, ok := bestNodes[nid]; !ok || 1 < existing.hop {
				bestNodes[nid] = nodeClaim{peer.Sender, peer.Subnet, 1}
			}
		}
		for _, cs := range peer.ClaimedSubnets {
			if cs.NodeID == m.nodeID || cs.NodeID == "" {
				continue
			}
			if existing, ok := bestNodes[cs.NodeID]; !ok || cs.Hop < existing.hop {
				bestNodes[cs.NodeID] = nodeClaim{peer.Sender, cs.Subnet, cs.Hop}
			}
		}
	}
	for nid, entry := range bestNodes {
		domain := NodeDomain(nid)
		var senderStr string
		if entry.sender == nil {
			senderStr = "self"
		} else {
			senderStr = entry.sender.GetNodeID()
		}
		var subnetStr string
		if entry.subnet != nil {
			subnetStr = entry.subnet.String()
		}
		util.LogDebug("[MESH] auto-insert: %s → sender=%s subnet=%s hop=%d", domain, senderStr, subnetStr, entry.hop)
		trie.Insert(domain, entry.sender, entry.subnet, entry.hop)
	}

	// Atomically swap in the new route table (lock-free for readers)
	m.routeTable.Store(&routeTable{
		routes:     routes,
		domainTrie: trie,
	})
	util.LogInfo("[MESH] routes installed: %d routes", len(routes))
	for _, r := range routes {
		var peerIDs []string
		for _, p := range r.Peers {
			peerIDs = append(peerIDs, fmt.Sprintf("%s(hop=%d)", p.Peer.GetNodeID(), p.Hop))
		}
		util.LogDebug("[MESH]   %s -> %s", r.Prefix, strings.Join(peerIDs, ", "))
	}
	// Log domain suffixes for debugging
	util.LogInfo("[MESH] domain trie: own suffixes=%v", domainSuffixes)
	for _, peer := range peers {
		if peer.Sender == nil {
			continue
		}
		var suffixes []string
		for _, entry := range peer.DomainSuffixes {
			suffixes = append(suffixes, entry.Suffix)
		}
		if len(suffixes) > 0 {
			util.LogInfo("[MESH] domain trie: peer %s suffixes=%v", peer.Sender.GetNodeID(), suffixes)
		}
	}
	// Log auto-generated nodeID.phn entries
	var autoDomains []string
	for nid, entry := range bestNodes {
		if entry.sender == nil {
			autoDomains = append(autoDomains, fmt.Sprintf("%s.phn(self)", nid))
		} else {
			autoDomains = append(autoDomains, fmt.Sprintf("%s.phn(→%s,hop=%d)", nid, entry.sender.GetNodeID(), entry.hop))
		}
	}
	util.LogInfo("[MESH] domain trie: auto nodeID.phn entries=%v", autoDomains)
}

// findRoute returns the MeshRoute matching dstIP, or nil if no match.
// The returned pointer allows updating lastIdx for round-robin selection.
func (m *MeshManager) findRoute(dstIP net.IP) *MeshRoute {
	rt := m.getRouteTable()

	for i := range rt.routes {
		if rt.routes[i].Prefix.Contains(dstIP) {
			if isMeshAddress(dstIP) {
				util.LogDebug("[MESH] findRoute: dst=%s matched route prefix=%s peers=%d",
					dstIP, rt.routes[i].Prefix, len(rt.routes[i].Peers))
			}
			return &rt.routes[i]
		}
	}
	return nil
}

func (m *MeshManager) gossipLoop() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.closeCh:
			return
		case <-ticker.C:
			m.broadcastGossip()
		case ev := <-m.eventCh:
			switch ev.kind {
			case meshEventRegister:
				m.topology.RegisterPeer(ev.sender)
				m.recomputeRoutes()
				util.DefaultVersionNotifier.BumpVersion("mesh")
				util.LogInfo("[MESH] peer registered: %s", ev.sender.GetNodeID())
			case meshEventUnregister:
				nodeID := ev.sender.GetNodeID()
				m.topology.UnregisterPeer(ev.sender)
				m.recomputeRoutes()
				util.DefaultVersionNotifier.BumpVersion("mesh")
				util.LogInfo("[MESH] peer unregistered: %s", nodeID)
			case meshEventGossip:
				var info GossipInfo
				if err := json.Unmarshal(ev.data, &info); err != nil {
					util.LogDebug("[MESH] bad gossip from %s: %v", ev.sender.GetNodeID(), err)
					break
				}
				util.LogDebug("[MESH] gossip from %s: subnet=%s routes=%d domainSuffixes=%d claimedSubnets=%d", ev.sender.GetNodeID(), info.Subnet, len(info.Routes), len(info.DomainSuffixes), len(info.ClaimedSubnets))
				if m.topology.UpdateGossip(ev.sender, info) {
					m.recomputeRoutes()
					util.DefaultVersionNotifier.BumpVersion("mesh")
				}
				// Check for subnet conflicts and re-select if needed
				if m.checkSubnetConflict() {
					m.recomputeRoutes()
					m.broadcastGossip()
				}
			case meshEventConfigUpdate:
				m.recomputeRoutes()
				m.broadcastGossip()
				util.DefaultVersionNotifier.BumpVersion("mesh")
				m.mu.RLock()
				util.LogInfo("[MESH] config updated: domainSuffixes=%v advertise=%v", m.domainSuffixes, m.advertise)
				m.mu.RUnlock()
			}
		}
	}
}

type meshEventKind int

const (
	meshEventRegister meshEventKind = iota
	meshEventUnregister
	meshEventGossip
	meshEventTick
	meshEventConfigUpdate
)

type meshEvent struct {
	kind   meshEventKind
	sender PeerSender
	data   []byte
}

func (m *MeshManager) broadcastGossip() {
	if m.p2p == nil {
		util.LogDebug("[MESH] broadcastGossip: p2p is nil")
		return
	}
	allPeers := m.topology.GetAllPeers()
	if len(allPeers) == 0 {
		util.LogDebug("[MESH] broadcastGossip: no peers")
		return
	}
	util.LogDebug("[MESH] broadcastGossip: sending to %d peers", len(allPeers))

	m.mu.RLock()
	advertise := make([]string, len(m.advertise))
	copy(advertise, m.advertise)
	domainSuffixes := make([]string, len(m.domainSuffixes))
	copy(domainSuffixes, m.domainSuffixes)
	m.mu.RUnlock()

	// Build global route table (non-mesh routes only)
	type globalRouteEntry struct {
		hop     int
		nextHop *PeerInfo // nil = own route
		prefix  string
		ipNet   *net.IPNet
	}
	bestRoutes := make(map[string]globalRouteEntry)

	// Own advertise routes (Hop=0) — not mesh subnets
	for _, r := range advertise {
		_, ipNet, _ := net.ParseCIDR(r)
		bestRoutes[r] = globalRouteEntry{0, nil, r, ipNet}
	}
	// Peer routes (non-mesh, learned from peers)
	for _, peer := range allPeers {
		if peer.Sender == nil {
			continue
		}
		for _, r := range peer.Routes {
			if existing, ok := bestRoutes[r.PrefixStr]; !ok || r.Hop < existing.hop {
				bestRoutes[r.PrefixStr] = globalRouteEntry{r.Hop, peer, r.PrefixStr, r.Prefix}
			}
		}
	}

	// Build global claimed subnets table (mesh subnets)
	type globalClaimEntry struct {
		hop     int
		nextHop *PeerInfo // nil = own claim
		nodeID  string
		subnet  string
	}
	bestClaims := make(map[string]globalClaimEntry)

	// Own claim (Hop=0)
	bestClaims[m.subnetStr] = globalClaimEntry{0, nil, m.nodeID, m.subnetStr}
	// Peer claims
	for _, peer := range allPeers {
		if peer.Sender == nil {
			continue
		}
		// Peer's own subnet
		if peer.SubnetStr != "" {
			key := peer.SubnetStr
			if existing, ok := bestClaims[key]; !ok || 1 < existing.hop {
				bestClaims[key] = globalClaimEntry{1, peer, peer.NodeID(), peer.SubnetStr}
			}
		}
		// Peer's learned claims
		for _, cs := range peer.ClaimedSubnets {
			if existing, ok := bestClaims[cs.SubnetStr]; !ok || cs.Hop < existing.hop {
				bestClaims[cs.SubnetStr] = globalClaimEntry{cs.Hop, peer, cs.NodeID, cs.SubnetStr}
			}
		}
	}

	// Build global domain suffix map
	type globalDSEntry struct {
		hop     int
		nextHop *PeerInfo // nil = own entry
		subnet  string    // Fake-IP subnet of the owning node
	}
	bestDS := make(map[string]globalDSEntry)
	for _, s := range domainSuffixes {
		bestDS[s] = globalDSEntry{0, nil, m.subnetStr}
	}
	for _, peer := range allPeers {
		if peer.Sender == nil {
			continue
		}
		for _, entry := range peer.DomainSuffixes {
			if existing, ok := bestDS[entry.Suffix]; !ok || entry.Hop < existing.hop {
				bestDS[entry.Suffix] = globalDSEntry{entry.Hop, peer, entry.SubnetStr}
			}
		}
	}

	// Build global topology edge set (deduplicated)
	type globalEdgeEntry struct {
		nextHop  *PeerInfo // nil = own observation
		nodeID   string
		neighbor string
	}
	bestEdges := make(map[string]globalEdgeEntry) // key: "nodeA|nodeB" (sorted)

	// Own direct edges (to all peers)
	for _, peer := range allPeers {
		if peer.Sender == nil {
			continue
		}
		key := edgeKey(m.nodeID, peer.NodeID())
		if _, exists := bestEdges[key]; !exists {
			bestEdges[key] = globalEdgeEntry{nil, m.nodeID, peer.NodeID()}
		}
	}

	// Edges learned from peers
	for _, peer := range allPeers {
		if peer.Sender == nil {
			continue
		}
		for _, e := range peer.TopologyEdges {
			key := edgeKey(e.NodeID, e.Neighbor)
			if _, exists := bestEdges[key]; !exists {
				bestEdges[key] = globalEdgeEntry{peer, e.NodeID, e.Neighbor}
			}
		}
	}

	// Per-peer: filter by split horizon and send
	// Iterate over all peers (including multiple connections to same node)
	for _, peer := range allPeers {
		if peer.Sender == nil {
			continue
		}

		// Filter routes: exclude entries where nextHop == this peer
		var routes []GossipRoute
		for _, e := range bestRoutes {
			if e.nextHop == peer {
				continue // split horizon
			}
			routes = append(routes, GossipRoute{Prefix: e.prefix, Hop: e.hop})
		}

		// Filter domain suffixes: exclude entries where nextHop == this peer
		var ds []GossipDomainSuffix
		for suffix, e := range bestDS {
			if e.nextHop == peer {
				continue // split horizon
			}
			ds = append(ds, GossipDomainSuffix{Suffix: suffix, Subnet: e.subnet, Hop: e.hop})
		}

		// Filter claimed subnets: exclude entries where nextHop == this peer
		var claims []GossipClaimedSubnet
		for _, e := range bestClaims {
			if e.nextHop == peer {
				continue // split horizon
			}
			claims = append(claims, GossipClaimedSubnet{
				Subnet: e.subnet,
				NodeID: e.nodeID,
				Hop:    e.hop,
			})
		}

		// Filter topology edges: exclude entries learned from this peer,
		// and entries involving this peer (they already know)
		var edges []GossipTopologyEdge
		for _, e := range bestEdges {
			if e.nextHop == peer {
				continue // split horizon
			}
			if e.nodeID == peer.NodeID() || e.neighbor == peer.NodeID() {
				continue // peer already knows about its own edges
			}
			edges = append(edges, GossipTopologyEdge{
				NodeID:   e.nodeID,
				Neighbor: e.neighbor,
			})
		}

		info := GossipInfo{
			NodeID:         m.nodeID,
			Subnet:         m.subnetStr,
			DomainSuffixes: ds,
			Routes:         routes,
			ClaimedSubnets: claims,
			TopologyEdges:  edges,
		}
		data, err := json.Marshal(info)
		if err != nil {
			continue
		}
		peer.Sender.SendGossip(data)
	}
}

// edgeKey returns a canonical key for an edge between two nodes (sorted order).
func edgeKey(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return a + "|" + b
}

// rewriteSrcIPInPacket rewrites the source IP in a raw IPv4 packet
// and recomputes the IP header checksum. Used as a fallback when natTable is nil.
func rewriteSrcIPInPacket(pkt []byte, newSrc net.IP) []byte {
	if len(pkt) < 20 || pkt[0]>>4 != 4 {
		return nil
	}
	result := make([]byte, len(pkt))
	copy(result, pkt)
	copy(result[12:16], newSrc.To4())
	result[10] = 0
	result[11] = 0
	var sum uint32
	headerLen := int(result[0]&0x0f) * 4
	for i := 0; i < headerLen-1; i += 2 {
		sum += uint32(result[i])<<8 | uint32(result[i+1])
	}
	for sum>>16 > 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	cksum := ^uint16(sum)
	result[10] = byte(cksum >> 8)
	result[11] = byte(cksum)
	return result
}
