package mesh

import (
	"encoding/json"
	"fmt"
	"math/rand"
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

func tcpSYNInfo(packet []byte) (net.IP, net.IP, uint16, bool) {
	if len(packet) < 40 || packet[0]>>4 != 4 || packet[9] != 6 {
		return nil, nil, 0, false
	}
	headerLen := int(packet[0]&0x0f) * 4
	if headerLen < 20 || len(packet) < headerLen+14 {
		return nil, nil, 0, false
	}
	flags := packet[headerLen+13]
	if flags&0x02 == 0 || flags&0x10 != 0 {
		return nil, nil, 0, false
	}
	return net.IP(packet[12:16]), net.IP(packet[16:20]), uint16(packet[headerLen+2])<<8 | uint16(packet[headerLen+3]), true
}

func dnsPacketInfo(packet []byte) (net.IP, net.IP, uint16, bool) {
	if len(packet) < 28 || packet[0]>>4 != 4 || packet[9] != 17 {
		return nil, nil, 0, false
	}
	headerLen := int(packet[0]&0x0f) * 4
	if headerLen < 20 || len(packet) < headerLen+8 {
		return nil, nil, 0, false
	}
	dstPort := uint16(packet[headerLen+2])<<8 | uint16(packet[headerLen+3])
	if dstPort != 53 {
		return nil, nil, 0, false
	}
	return net.IP(packet[12:16]), net.IP(packet[16:20]), dstPort, true
}

// TunInterface abstracts the TUN engine for mesh packet injection.
type TunInterface interface {
	// InjectFromNode injects a frame received from the given peer into the
	// netstack via that peer's Link NIC (design §5.2).
	InjectFromNode(nodeID string, data []byte) error
	WriteMeshPacket(data []byte) error
	GetNetstack() *Netstack
}

// PeerSender sends data directly to a connected peer.
type PeerSender interface {
	Send(data []byte) error
	SendGossip(data []byte)
	GetNodeID() string
	GetProxyName() string    // proxy name (link identifier)
	GetLinkID() string       // negotiated link ID (seq1-seq2 sorted, internal use)
	GetLocalSeq() uint16     // local sequence number (for gossip advertisement)
	GetRemoteSeq() uint16    // remote sequence number (for gossip advertisement)
	GetFriendlyName() string // friendly name (proxy name, for display only)
}

// P2PTransport abstracts the P2P layer for mesh packet delivery.
type P2PTransport interface {
	BroadcastMeshGossip(data []byte) error
	SendMeshGossipTo(peerNodeID string, data []byte) error
	SendMeshGossipToAll(data []byte)
	ListMeshPeerIDs() []string
	SetMeshInfo(nodeID, vip string)
	ResendHelloToAll()
	StopPeerByNodeID(nodeID string)
	StopPeerByProxyName(proxyName string)
	StopPeerByLinkID(linkID string)
	GetLinkQualityStats(nodeID string) (srtt, rto, jitter time.Duration, lossRate float64, sampleCount int)
	GetLinkQualityStatsByProxy(proxyName string) (srtt, rto, jitter time.Duration, lossRate float64, sampleCount int)
	GetLinkQualityStatsByLinkID(linkID string) (srtt, rto, jitter time.Duration, lossRate float64, sampleCount int)
}

// MeshPeerInfo describes a connected mesh peer.
type MeshPeerInfo struct {
	NodeID      string    `json:"nodeId"`
	Subnet      string    `json:"subnet"`
	Direct      bool      `json:"direct"`
	LastSeen    time.Time `json:"lastSeen"`
	SRTT        float64   `json:"srtt,omitempty"`        // smoothed RTT in milliseconds
	Jitter      float64   `json:"jitter,omitempty"`      // RTT jitter in milliseconds
	SampleCount int       `json:"sampleCount,omitempty"` // number of RTT samples
	LossRate    float64   `json:"lossRate,omitempty"`    // loss rate (0.0-1.0)
}

// PeerWithHop pairs a peer sender with its hop count.
type PeerWithHop struct {
	Peer PeerSender
	Hop  int
}

// MeshRoute represents a route to a network prefix.
// Entries contains the egress nodes that own/advertise this route (unified static+dynamic).
// Next hop is looked up from topology when sending, not stored here.
type MeshRoute struct {
	Prefix  *net.IPNet
	Entries []RouteEntry // unified entries with nodeID + source tag
}

// RouteSource indicates the origin of a route entry.
type RouteSource string

const (
	RouteSourceStatic  RouteSource = "static"
	RouteSourceDynamic RouteSource = "dynamic"
)

// RouteEntry represents a single egress node for a route, with source tracking.
type RouteEntry struct {
	NodeID string
	Source RouteSource
}

// selectEgressNodeID selects the best egress nodeID from a list of route entries.
// Algorithm:
// 1. Sort: static entries first, dynamic entries after
// 2. Sticky: if previously selected node is still available, keep it
// 3. Hash: stable selection based on targetIP (first time only)
//
// No online filter here: entries come from the unified route table which is
// recomputed on topology changes, so an owner present in Entries is reachable
// via topology (possibly multi-hop). isNodeOnline only knows direct peers and
// would wrongly drop topology-advertised owners.
func (m *MeshManager) selectEgressNodeID(targetIP net.IP, entries []RouteEntry) string {
	if len(entries) == 0 {
		return ""
	}

	// Sort: static first, then dynamic
	sorted := make([]RouteEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Source == RouteSourceStatic && sorted[j].Source == RouteSourceDynamic {
			return true
		}
		if sorted[i].Source == RouteSourceDynamic && sorted[j].Source == RouteSourceStatic {
			return false
		}
		return false // maintain original order within same source
	})

	// Skip self: own prefixes never reach here via nextHops, but be safe.
	available := make([]RouteEntry, 0, len(sorted))
	for _, e := range sorted {
		if e.NodeID != m.nodeID {
			available = append(available, e)
		}
	}

	if len(available) == 0 {
		return ""
	}

	// Sticky: check if we have a cached selection for this target
	targetKey := targetIP.String()
	m.stickyMu.Lock()
	if cachedNodeID, ok := m.stickyCache[targetKey]; ok {
		// Verify cached node is still in available list
		for _, e := range available {
			if e.NodeID == cachedNodeID {
				m.stickyMu.Unlock()
				util.LogDebug("[MESH] sticky IP route: %s → %s (cached)", targetIP, cachedNodeID)
				return cachedNodeID
			}
		}
		// Cached node no longer available, will re-select below
		util.LogDebug("[MESH] sticky IP route: %s → %s no longer available, re-selecting", targetIP, cachedNodeID)
	}
	m.stickyMu.Unlock()

	// Hash-based stable selection
	hash := 0
	for _, b := range targetIP {
		hash = hash*31 + int(b)
	}
	if hash < 0 {
		hash = -hash
	}
	idx := hash % len(available)
	selectedNodeID := available[idx].NodeID

	// Cache the selection
	m.stickyMu.Lock()
	m.stickyCache[targetKey] = selectedNodeID
	m.stickyMu.Unlock()

	util.LogDebug("[MESH] sticky IP route: %s → %s (new selection)", targetIP, selectedNodeID)
	return selectedNodeID
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

	// routeTable holds routes and domain tries, accessed atomically for lock-free reads.
	// Writes create a new routeTable and Store() it atomically.
	routeTable atomic.Value // stores *routeTable

	DNSAllocator func(domain string) (net.IP, error)

	closeCh chan struct{}
	eventCh chan meshEvent

	// DNS hijacker and Fake-IP pool (mesh DNS service)
	dnsHijacker *DNSHijacker
	fakeIPPool  *FakeIPPool

	// IPIP tunnel for static policy routing
	ipipTunnel *IPIPTunnel

	// Route change callback for netstack integration
	onRouteChange        func()
	staticRoutes         []config.MeshStaticRoute
	staticDomainSuffixes []config.MeshStaticDomainSuffix

	// Link quality tracking
	qualityTracker *PeerQualityTracker

	// Round-robin state for multi-link load balancing (design §2.8)
	// Key: target nodeID, Value: last used index
	linkSelectIdx map[string]int
	linkSelectMu  sync.Mutex

	// Sticky node selection cache (target → nodeID)
	// Ensures stable routing: once a nodeID is selected for a target, keep using it
	// until the node becomes unavailable.
	stickyMu    sync.Mutex
	stickyCache map[string]string // target (IP/domain) → nodeID

	// TCP keepalive settings for mesh connections
	tcpKeepalive *config.MeshTCPKeepalive

	// OnPeerRegistered is called when a new peer is registered (for package sync)
	OnPeerRegistered func(nodeID string)
}

// nodeInfo stores information about a discovered node (from claimedSubnets).
type nodeInfo struct {
	sender PeerSender // nil for own node
	subnet *net.IPNet
	hop    int
}

// routeTable is an immutable snapshot of routing state, swapped atomically.
type routeTable struct {
	routes     []MeshRoute               // sorted by prefix length (longest first)
	domainTrie *NodeTrie                 // unified domain routes (static + dynamic merged)
	nodeMap    map[string]*nodeInfo      // nodeID → info
	bestPaths  map[string]dijkstraResult // Dijkstra-computed best paths: nodeID -> (nextHop, cost)
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
		eventCh:         make(chan meshEvent, 1024),
		stickyCache:     make(map[string]string),
		linkSelectIdx:   make(map[string]int),
	}
	// Initialize routeTable with empty routes
	m.routeTable.Store(&routeTable{
		routes:     make([]MeshRoute, 0),
		domainTrie: NewNodeTrie(),
		nodeMap:    make(map[string]*nodeInfo),
	})

	// Create Fake-IP pool from node subnet (skip first 10: .0=network, .1=VIP, .2=hostIP, .3=GIP, .4=EIP, .5-.9=future)
	m.fakeIPPool = NewFakeIPPoolWithSubnet(subnet, 9)

	// Load persistent FakeIP mappings from database
	m.fakeIPPool.LoadFromDB()

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

	// Create link quality tracker
	m.qualityTracker = NewPeerQualityTracker()

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

// SendRawPacket sends a raw IP packet via the mesh network.
// This is used by IPIP transit forwarding at the frame layer (HandleMeshFrame).
func (m *MeshManager) SendRawPacket(data []byte) error {
	if len(data) < 20 {
		return fmt.Errorf("packet too short: %d bytes", len(data))
	}

	// Extract destination IP from the packet
	var dstIP net.IP
	version := data[0] >> 4
	if version == 4 {
		dstIP = net.IP(data[16:20])
	} else {
		return fmt.Errorf("unsupported IP version: %d", version)
	}

	// Find the route for the destination
	nextHops := m.findNextHops(dstIP)
	if len(nextHops) == 0 {
		return fmt.Errorf("no next hops for %s", dstIP)
	}

	// Select the best peer
	candidatePeers := m.selectBestPeers(nextHops, dstIP)
	if len(candidatePeers) == 0 {
		return fmt.Errorf("no candidate peers for %s", dstIP)
	}

	// Try to send via each candidate peer
	for _, peer := range candidatePeers {
		if err := peer.Send(data); err == nil {
			return nil
		}
	}

	return fmt.Errorf("failed to send packet via any peer")
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
// Returns (nodeIDs, true) if matched, (nil, false) otherwise.
func (m *MeshManager) CheckStaticRoute(dstIP net.IP) ([]string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.ipipTunnel == nil {
		return nil, false
	}
	// Special logging for 8.8.8.0/24 range (our test destination)
	if len(dstIP) >= 4 && dstIP[0] == 8 && dstIP[1] == 8 && dstIP[2] == 8 {
		util.LogDebug("[IPIP] CheckStaticRoute for 8.8.8.x: dst=%s, routes=%d", dstIP, len(m.staticRoutes))
	}
	return m.ipipTunnel.MatchStaticRoute(dstIP, m.staticRoutes)
}

// CheckStaticDomainSuffix checks if a domain matches any static domain suffix route.
// Returns (nodeIDs, true) if matched, (nil, false) otherwise.
func (m *MeshManager) CheckStaticDomainSuffix(domain string) ([]string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.ipipTunnel == nil {
		return nil, false
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

// CloseCh returns the close channel for cleanup.
func (m *MeshManager) CloseCh() <-chan struct{} {
	return m.closeCh
}

func (m *MeshManager) GetAllVIPs() []net.IP {
	return []net.IP{m.vip}
}

func (m *MeshManager) GetSubnet() string {
	return m.subnetStr
}

func (m *MeshManager) GetNetwork() *net.IPNet {
	return m.network
}

func (m *MeshManager) GetDomainSuffixes() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]string, len(m.domainSuffixes))
	copy(result, m.domainSuffixes)
	return result
}

// GetRouteTable returns the current mesh route table (exported for netstack integration).
func (m *MeshManager) GetRouteTable() *RouteTableSnapshot {
	rt := m.getRouteTable()
	if rt == nil {
		return nil
	}
	// Convert to snapshot for external use
	snapshot := &RouteTableSnapshot{
		Routes: make([]MeshRoute, len(rt.routes)),
	}
	copy(snapshot.Routes, rt.routes)
	return snapshot
}

// RouteTableSnapshot is a snapshot of the route table for external use.
type RouteTableSnapshot struct {
	Routes []MeshRoute
}

// GetMeshSubnet returns the mesh subnet (100.64.0.0/10).
func (m *MeshManager) GetMeshSubnet() *net.IPNet {
	return m.network
}

// SelectEgressNodeID selects the best egress node for a given target IP (exported).
func (m *MeshManager) SelectEgressNodeID(targetIP net.IP, entries []RouteEntry) string {
	return m.selectEgressNodeID(targetIP, entries)
}

// GetEIPForNode returns the EIP for a given node ID (exported).
func (m *MeshManager) GetEIPForNode(nodeID string) net.IP {
	return m.getEIPForNode(nodeID)
}

// NextHopNodeID returns the next-hop peer node ID toward the target node per
// the Dijkstra best path (design §5.2). For direct peers this is the peer
// itself. Returns "" when no path is known.
func (m *MeshManager) NextHopNodeID(targetNodeID string) string {
	rt := m.getRouteTable()
	if rt != nil {
		if path, ok := rt.bestPaths[targetNodeID]; ok && path.NextHop != "" {
			return path.NextHop
		}
	}
	// Topology not converged yet: a direct peer is its own next hop.
	for _, peer := range m.topology.GetAllPeers() {
		if peer.NodeID() == targetNodeID && peer.Sender != nil {
			return targetNodeID
		}
	}
	return ""
}

// ResolveNodeIDForIP finds the egress node for ip via the mesh route table
// (longest matching prefix, design §5.2). Returns "" when no prefix matches.
func (m *MeshManager) ResolveNodeIDForIP(ip net.IP) string {
	rt := m.GetRouteTable()
	if rt == nil {
		return ""
	}
	var best *MeshRoute
	bestOnes := -1
	for i := range rt.Routes {
		r := &rt.Routes[i]
		if r.Prefix.Contains(ip) {
			ones, _ := r.Prefix.Mask.Size()
			if best == nil || ones > bestOnes {
				best = r
				bestOnes = ones
			}
		}
	}
	if best == nil {
		return ""
	}
	return m.SelectEgressNodeID(ip, best.Entries)
}

// SetOnRouteChange sets a callback to be invoked when the route table changes.
func (m *MeshManager) SetOnRouteChange(callback func()) {
	m.onRouteChange = callback
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
		util.LogWarn("[MESH] eventCh full, dropping config update event")
	}
}

// TriggerGossip sends an immediate gossip broadcast.
func (m *MeshManager) TriggerGossip() {
	select {
	case m.eventCh <- meshEvent{kind: meshEventTick}:
	default:
		util.LogWarn("[MESH] eventCh full, dropping trigger gossip event")
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

// GetVIPForNode returns the VIP for a given node ID from the topology.
// Public wrapper for getVIPForNode (used by RouteSelector).
func (m *MeshManager) GetVIPForNode(nodeID string) net.IP {
	return m.getVIPForNode(nodeID)
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
	// Look up in topology: direct peer's own subnet first
	for _, peer := range m.topology.GetAllPeers() {
		if peer.NodeID() == nodeID && peer.Subnet != nil {
			return peer.Subnet
		}
	}
	// Then in claimed subnets advertised via gossip (multi-hop owners)
	for _, peer := range m.topology.GetAllPeers() {
		for _, cs := range peer.ClaimedSubnets {
			if cs.NodeID == nodeID {
				return cs.Subnet
			}
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
	// Set TCP keepalive on netstack if available
	if tun != nil && tun.GetNetstack() != nil && m.tcpKeepalive != nil {
		tun.GetNetstack().SetTCPKeepalive(m.tcpKeepalive)
	}
}

// SetTCPKeepalive sets the TCP keepalive settings for mesh connections.
func (m *MeshManager) SetTCPKeepalive(ka *config.MeshTCPKeepalive) {
	m.tcpKeepalive = ka
	// If TUN is already set, apply immediately
	if m.tun != nil && m.tun.GetNetstack() != nil && ka != nil {
		m.tun.GetNetstack().SetTCPKeepalive(ka)
	}
}

var GlobalMeshManager *MeshManager

func (m *MeshManager) Start(tun TunInterface, p2p P2PTransport) {
	m.tun = tun
	m.p2p = p2p
	// Set mesh info on P2P layer so hello messages include correct meshNodeId
	p2p.SetMeshInfo(m.nodeID, m.vip.String())
	m.recomputeRoutes()
	go m.gossipLoop()
	go m.qualityLoop()
	util.LogInfo("[MESH] started: nodeID=%s vip=%s subnet=%s subnetStr=%s", m.nodeID, m.vip, m.subnet, m.subnetStr)
}

func (m *MeshManager) Stop() {
	close(m.closeCh)
}

// ResolveDomainSubnet looks up a domain in the unified domain route trie.
// Returns (subnet, needsFail):
//   - subnet != nil: matched a remote node, forward DNS query to that node
//   - subnet == nil && needsFail == false: local domain or no match, use local Fake-IP pool
func (m *MeshManager) ResolveDomainSubnet(domain string) (*net.IPNet, bool) {
	rt := m.getRouteTable()

	// Lookup in unified domain trie (contains both static and dynamic entries)
	entries, matchLen := rt.domainTrie.Lookup(domain)
	if matchLen == 0 || len(entries) == 0 {
		// No match → use local pool
		return nil, false
	}

	util.LogInfo("[MESH] ResolveDomainSubnet(%s): match entries=%d len=%d", domain, len(entries), matchLen)
	for i, e := range entries {
		util.LogInfo("[MESH] ResolveDomainSubnet(%s): entry[%d] nodeID=%s source=%v", domain, i, e.NodeID, e.Source)
	}

	// Select target node (static priority + hash stability)
	selectedNodeID := m.selectEgressNodeIDForDomain(domain, entries)
	util.LogInfo("[MESH] ResolveDomainSubnet(%s): selectedNodeID=%s", domain, selectedNodeID)
	if selectedNodeID == "" {
		// All entries were for self → this is a local domain
		// Allocate from local Fake-IP pool
		util.LogDebug("[MESH] ResolveDomainSubnet(%s): local domain, use local pool", domain)
		return nil, false
	}

	// Look up node in nodeMap (should always exist if domain trie is consistent)
	nodeInfo := rt.nodeMap[selectedNodeID]
	if nodeInfo == nil || nodeInfo.subnet == nil {
		// This shouldn't happen if domain trie and nodeMap are in sync
		util.LogWarn("[MESH] ResolveDomainSubnet(%s): node %s not in nodeMap, inconsistent state", domain, selectedNodeID)
		return nil, false
	}

	// Local node (sender == nil) → use local pool
	if nodeInfo.sender == nil {
		util.LogDebug("[MESH] ResolveDomainSubnet(%s): node %s is local, use local pool", domain, selectedNodeID)
		return nil, false
	}

	util.LogDebug("[MESH] ResolveDomainSubnet(%s): forward to node %s subnet %s", domain, selectedNodeID, nodeInfo.subnet)
	return nodeInfo.subnet, false
}

// selectEgressNodeIDForDomain selects the best egress nodeID for domain routing.
// Uses the same unified algorithm as IP routing: static priority + hash stability + sticky cache.
// Domain trie entries are generated from topology, so if an entry exists, the node is reachable.
func (m *MeshManager) selectEgressNodeIDForDomain(domain string, entries []RouteEntry) string {
	if len(entries) == 0 {
		return ""
	}

	// Filter: skip self
	var available []RouteEntry
	for _, e := range entries {
		if e.NodeID != m.nodeID {
			available = append(available, e)
		}
	}

	if len(available) == 0 {
		return ""
	}

	// Sort: static first, then dynamic
	sort.SliceStable(available, func(i, j int) bool {
		if available[i].Source == RouteSourceStatic && available[j].Source == RouteSourceDynamic {
			return true
		}
		return false
	})

	// Sticky: check if we have a cached selection for this domain
	m.stickyMu.Lock()
	if cachedNodeID, ok := m.stickyCache[domain]; ok {
		// Verify cached node is still in available list
		for _, e := range available {
			if e.NodeID == cachedNodeID {
				m.stickyMu.Unlock()
				util.LogDebug("[MESH] sticky domain route: %s → %s (cached)", domain, cachedNodeID)
				return cachedNodeID
			}
		}
		// Cached node no longer available, will re-select below
		util.LogDebug("[MESH] sticky domain route: %s → %s no longer available, re-selecting", domain, cachedNodeID)
	}
	m.stickyMu.Unlock()

	// Hash-based stable selection using domain name
	hash := 0
	for _, c := range domain {
		hash = hash*31 + int(c)
	}
	if hash < 0 {
		hash = -hash
	}
	idx := hash % len(available)
	selectedNodeID := available[idx].NodeID

	// Cache the selection
	m.stickyMu.Lock()
	m.stickyCache[domain] = selectedNodeID
	m.stickyMu.Unlock()

	util.LogDebug("[MESH] sticky domain route: %s → %s (new selection)", domain, selectedNodeID)
	return selectedNodeID
}

// RegisterPeer is called when a P2P peer with mesh capability connects.
func (m *MeshManager) RegisterPeer(sender PeerSender) {
	select {
	case m.eventCh <- meshEvent{kind: meshEventRegister, sender: sender}:
	default:
		util.LogWarn("[MESH] eventCh full, dropping peer register event for %s", sender.GetNodeID())
	}
}

// UnregisterPeer is called when a P2P peer disconnects.
func (m *MeshManager) UnregisterPeer(sender PeerSender) {
	select {
	case m.eventCh <- meshEvent{kind: meshEventUnregister, sender: sender}:
	default:
		util.LogWarn("[MESH] eventCh full, dropping peer unregister event for %s", sender.GetNodeID())
	}
}

// UnregisterPeerByNodeID removes a peer by nodeID (used when re-registering after hello).
func (m *MeshManager) UnregisterPeerByNodeID(nodeID string) {
	m.topology.UnregisterPeerByNodeID(nodeID)
}

// SendToNode sends an encapsulated packet to a specific target node via the mesh network.
// This is used by tunnel NICs to send IPIP-encapsulated packets to egress nodes.
// Multi-link load balancing: round-robin among same-hop peers (design §2.8).
func (m *MeshManager) SendToNode(targetNodeID string, packet []byte) error {
	if targetNodeID == "" {
		return fmt.Errorf("empty target node ID")
	}
	if len(packet) == 0 {
		return fmt.Errorf("empty packet")
	}

	// Find peers that can reach the target node
	nextHops := m.findNextHopsForNode(targetNodeID)
	if len(nextHops) == 0 {
		return fmt.Errorf("no route to node %s", targetNodeID)
	}

	// Find minimum hop count
	minHop := nextHops[0].Hop

	// Collect all peers with minimum hop count
	var sameHopPeers []PeerWithHop
	for _, peerWithHop := range nextHops {
		if peerWithHop.Hop == minHop {
			sameHopPeers = append(sameHopPeers, peerWithHop)
		} else {
			break // Already sorted by hop, so we can stop
		}
	}

	// Round-robin selection among same-hop peers (design §2.8)
	m.linkSelectMu.Lock()
	idx := m.linkSelectIdx[targetNodeID] % len(sameHopPeers)
	m.linkSelectIdx[targetNodeID] = idx + 1
	m.linkSelectMu.Unlock()

	// Try to send via selected peer, with failover to other same-hop peers
	var sendErr error
	for i := 0; i < len(sameHopPeers); i++ {
		peerIdx := (idx + i) % len(sameHopPeers)
		peerWithHop := sameHopPeers[peerIdx]
		peer := peerWithHop.Peer

		TraceForwarding("peer_selected", packet, "target=%s peer=%s proxy=%s linkID=%s hop=%d candidate=%d/%d", targetNodeID, peer.GetNodeID(), peer.GetProxyName(), peer.GetLinkID(), peerWithHop.Hop, peerIdx+1, len(sameHopPeers))
		sendErr = peer.Send(packet)
		if sendErr == nil {
			TraceForwarding("peer_enqueued", packet, "target=%s peer=%s proxy=%s linkID=%s", targetNodeID, peer.GetNodeID(), peer.GetProxyName(), peer.GetLinkID())
			if i > 0 {
				util.LogInfo("[MESH] send to node %s: failed on first peer, succeeded on fallback peer %s (attempt %d)",
					targetNodeID, peer.GetNodeID(), i+1)
			}
			util.LogDebug("[MESH] sent %d bytes to node %s via peer %s (hop=%d, link=%d/%d)",
				len(packet), targetNodeID, peer.GetNodeID(), peerWithHop.Hop, peerIdx+1, len(sameHopPeers))
			return nil
		}

		TraceForwarding("peer_send_error", packet, "target=%s peer=%s proxy=%s linkID=%s err=%v", targetNodeID, peer.GetNodeID(), peer.GetProxyName(), peer.GetLinkID(), sendErr)

		// Handle peer stopped
		if strings.Contains(sendErr.Error(), "peer stopped") {
			util.LogWarn("[MESH] send to node %s failed: peer %s stopped, removing its selected link",
				targetNodeID, peer.GetNodeID())
			if m.p2p != nil {
				m.p2p.StopPeerByProxyName(peer.GetProxyName())
			}
			continue
		}

		// Queue full or other error, try next candidate
		util.LogDebug("[MESH] send to node %s via peer %s failed: %v, trying next candidate",
			targetNodeID, peer.GetNodeID(), sendErr)
	}

	// All same-hop peers failed, return error
	return fmt.Errorf("send to node %s failed on all %d same-hop candidates: %v", targetNodeID, len(sameHopPeers), sendErr)
}

// findNextHopsForNode finds peers that can reach a specific target node.
// Returns ALL peers to the best next hop (for multi-link load balancing).
func (m *MeshManager) findNextHopsForNode(targetNodeID string) []PeerWithHop {
	rt := m.getRouteTable()

	// Find the best path to the target node using Dijkstra results
	peers := m.topology.GetAllPeers()

	if path, ok := rt.bestPaths[targetNodeID]; ok {
		// Find ALL peers that connect to the best next hop node
		// (multiple links to same node = multiple peers with same NodeID)
		var result []PeerWithHop
		for _, peer := range peers {
			if peer.Sender != nil && peer.NodeID() == path.NextHop {
				result = append(result, PeerWithHop{Peer: peer.Sender, Hop: 1})
			}
		}
		if len(result) > 0 {
			return result
		}
	}

	// Fallback: find any peer that has the target node in its routing table
	return m.findNextHopsFallback([]string{targetNodeID}, peers)
}

// HandleMeshFrame processes a raw IP packet received from a peer (design §4.5,
// §5.2):
//   - IPIP frames (proto=4): decapsulate when the outer dst is our EIP
//     (tunnel terminator); otherwise relay the outer frame toward the egress
//     node (tunnel transit) without touching the inner packet.
//   - All other frames: inject into the fromNode's Link NIC. The netstack then
//     decides local delivery (GIP/VIP/fakeIP), forwarding (NAT return, relay),
//     TTL decrement and ICMP generation — no manual 卡口 at the frame layer.
func (m *MeshManager) HandleMeshFrame(fromNodeID string, frame []byte) {
	util.LogDebug("[MESH] HandleMeshFrame called from %s: %d bytes", fromNodeID, len(frame))
	if len(frame) < 20 || frame[0]>>4 != 4 {
		util.LogWarn("[MESH] bad packet from %s: %d bytes", fromNodeID, len(frame))
		return
	}

	// Check if this is an IPIP packet (protocol 4)
	if frame[9] == 4 {
		// Extract outer destination IP (IPIP header bytes 16-19)
		outerDstIP := net.IP(frame[16:20])
		localEIP := m.ipipTunnel.GetLocalEIP()

		// Check if outer destination is our EIP (we are the target)
		if localEIP != nil && outerDstIP.Equal(localEIP) {
			util.LogInfo("[IPIP] Received IPIP packet for us from %s, decapsulating", fromNodeID)
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

		// Not for us, forward the IPIP packet (we are a transit node)
		util.LogDebug("[IPIP] Forwarding IPIP packet from %s to %s", fromNodeID, outerDstIP)
		nextHops := m.findNextHops(outerDstIP)
		if len(nextHops) > 0 {
			// Select best peer using quality metrics (consider all candidates, not just min hop)
			nextPeer := m.selectBestPeer(nextHops, outerDstIP)
			if err := nextPeer.Send(frame); err != nil {
				util.LogWarn("[IPIP] Forward to %s failed: %v", nextPeer.GetNodeID(), err)
			} else {
				util.LogDebug("[IPIP] Forwarded IPIP packet to %s OK", nextPeer.GetNodeID())
			}
		} else {
			util.LogWarn("[IPIP] No route to forward IPIP packet to %s", outerDstIP)
		}
		return
	}

	dstIP := extractDstIP(frame)
	srcIP := net.IP(frame[12:16])
	if dstIP == nil {
		util.LogWarn("[MESH] bad packet from %s: cannot extract dst IP", fromNodeID)
		return
	}
	if len(frame) >= 20 && frame[9] == 6 {
		dstPort := uint16(frame[22])<<8 | uint16(frame[23])
		util.LogDebug("[MESH] recv TCP from %s: src=%s dst=%s:%d len=%d", fromNodeID, srcIP, dstIP, dstPort, len(frame))
	}
	util.LogDebug("[MESH] HandleMeshFrame from %s: src=%s dst=%s proto=%d TTL=%d len=%d",
		fromNodeID, srcIP, dstIP, frame[9], frame[8], len(frame))
	// Inject into the fromNode's Link NIC. Local delivery (GIP/VIP/fakeIP),
	// gateway forwarding (advertised prefixes → IPIP egress), mesh relay
	// (Link NIC → Link NIC), TTL and ICMP are all handled inside the netstack.
	if m.tun == nil {
		util.LogWarn("[MESH] no tun interface, dropping frame from %s (dst=%s)", fromNodeID, dstIP)
		return
	}
	TraceForwarding("mesh_inject_start", frame, "fromNode=%s", fromNodeID)
	if err := m.tun.InjectFromNode(fromNodeID, frame); err != nil {
		TraceForwarding("mesh_inject_error", frame, "fromNode=%s err=%v", fromNodeID, err)
		util.LogWarn("[MESH] inject frame from %s (dst=%s) via Link NIC failed: %v", fromNodeID, dstIP, err)
	} else {
		TraceForwarding("mesh_inject_ok", frame, "fromNode=%s", fromNodeID)
	}
}

// HandleTopologyGossip processes a gossip announcement from a peer.
func (m *MeshManager) HandleTopologyGossip(sender PeerSender, data []byte) {
	var info GossipInfo
	if err := json.Unmarshal(data, &info); err != nil {
		util.LogDebug("[MESH] bad gossip from %s: %v", sender.GetNodeID(), err)
		return
	}
	if sender.GetNodeID() == m.nodeID {
		return
	}
	select {
	case m.eventCh <- meshEvent{kind: meshEventGossip, sender: sender, data: data}:
	default:
		util.LogWarn("[MESH] eventCh full, dropping gossip from %s", sender.GetNodeID())
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
					"nodeId": r.NodeID,
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
	From  string             `json:"from"`
	To    string             `json:"to"`
	Links []FullTopologyLink `json:"links,omitempty"` // multiple links between same nodes
}

// FullTopologyLink represents a single link with quality metrics.
type FullTopologyLink struct {
	LocalSeq     uint16  `json:"localSeq"`               // local sequence number
	RemoteSeq    uint16  `json:"remoteSeq"`              // remote sequence number
	FriendlyName string  `json:"friendlyName,omitempty"` // display name (proxy name)
	SRTT         float64 `json:"srtt,omitempty"`         // smoothed RTT in milliseconds
	LossRate     float64 `json:"lossRate,omitempty"`     // loss rate (0.0-1.0)
	Cost         float64 `json:"cost,omitempty"`         // path cost
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

	// Build edge list: only local peer connections (active P2P connections)
	edgeSet := make(map[string]*FullTopologyEdge)

	// Local direct peer connections - only add edges for peers with active Senders
	for _, p := range peers {
		peerID := p.NodeID()
		// Only add edge if peer has an active Sender (indicating active P2P connection)
		if p.Sender != nil && peerID != "" {
			key := edgeKey(m.nodeID, peerID)

			// Get link info
			localSeq := p.Sender.GetLocalSeq()
			remoteSeq := p.Sender.GetRemoteSeq()
			friendlyName := p.Sender.GetFriendlyName()
			if friendlyName == "" {
				friendlyName = p.Sender.GetProxyName() // fallback
			}

			// Get link quality from qualityTracker
			var srtt, lossRate, jitter, cost float64
			if m.qualityTracker != nil {
				quality := m.qualityTracker.Get(peerID, localSeq)
				avgRTT, loss, jit := quality.Stats()
				srtt = float64(avgRTT.Milliseconds())
				lossRate = loss
				jitter = jit
				// Cost = SRTT * (1 + lossRate) + jitter
				cost = srtt*(1+lossRate) + jitter
			} else {
				cost = 1000
			}

			link := FullTopologyLink{
				LocalSeq:     localSeq,
				RemoteSeq:    remoteSeq,
				FriendlyName: friendlyName,
				SRTT:         srtt,
				LossRate:     lossRate,
				Cost:         cost,
			}

			if edge, exists := edgeSet[key]; exists {
				// Add link to existing edge
				edge.Links = append(edge.Links, link)
			} else {
				// Create new edge with first link
				edgeSet[key] = &FullTopologyEdge{
					From:  m.nodeID,
					To:    peerID,
					Links: []FullTopologyLink{link},
				}
			}
		}
	}

	// Learned edges from ClaimedSubnets.Neighbors
	// Each claim's Neighbors field lists the origin's direct neighbors with link quality
	for _, p := range peers {
		for _, cs := range p.ClaimedSubnets {
			for _, neighbor := range cs.Neighbors {
				key := edgeKey(cs.NodeID, neighbor.NodeID)

				// Convert GossipLink to FullTopologyLink
				var links []FullTopologyLink
				for _, gossipLink := range neighbor.Links {
					var cost float64
					if gossipLink.SRTT > 0 && gossipLink.LossRate < 1.0 {
						cost = gossipLink.SRTT / (1 - gossipLink.LossRate)
					} else if gossipLink.SRTT > 0 {
						cost = 10000 // high cost for high loss
					} else {
						cost = 1000 // default cost for no data
					}

					links = append(links, FullTopologyLink{
						LocalSeq:     gossipLink.LocalSeq,
						RemoteSeq:    gossipLink.RemoteSeq,
						FriendlyName: gossipLink.FriendlyName,
						SRTT:         gossipLink.SRTT,
						LossRate:     gossipLink.LossRate,
						Cost:         cost,
					})
				}

				if edge, exists := edgeSet[key]; exists {
					// Edge already exists (from local peers), add learned links
					// Deduplicate by seq pair
					for _, link := range links {
						linkExists := false
						for _, existingLink := range edge.Links {
							if existingLink.LocalSeq == link.LocalSeq && existingLink.RemoteSeq == link.RemoteSeq {
								linkExists = true
								break
							}
						}
						if !linkExists {
							edge.Links = append(edge.Links, link)
						}
					}
				} else {
					// Create new edge with learned links
					edgeSet[key] = &FullTopologyEdge{
						From:  cs.NodeID,
						To:    neighbor.NodeID,
						Links: links,
					}
				}
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
		edges = append(edges, *e)
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
		// Use findNextHops to get current next hop peers from topology
		// Use the first IP in the prefix for lookup
		if ip := r.Prefix.IP; ip != nil {
			if nextHops := m.findNextHops(ip); len(nextHops) > 0 {
				for _, p := range nextHops {
					peerInfos = append(peerInfos, map[string]interface{}{
						"nodeID": p.Peer.GetNodeID(),
						"hop":    p.Hop,
					})
				}
			}
		}
		routeList = append(routeList, map[string]interface{}{
			"prefix":  r.Prefix.String(),
			"entries": r.Entries,
			"via":     peerInfos,
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

		// Get RTT stats from the first link to this peer
		avgRTT, lossRate, jitter := m.GetPeerQuality(nodeID)
		_, _, _, _, sampleCount := m.p2p.GetLinkQualityStats(nodeID)

		result = append(result, MeshPeerInfo{
			NodeID:      nodeID,
			Direct:      true,
			Subnet:      p.SubnetStr,
			LastSeen:    p.LastSeen,
			SRTT:        float64(avgRTT.Milliseconds()),
			Jitter:      jitter,
			SampleCount: sampleCount,
			LossRate:    lossRate,
		})
	}
	return result
}

func (m *MeshManager) ResolveMeshDomain(domain string) net.IP {
	nodeID := ParseNodeDomain(domain)
	if nodeID == "" {
		return nil
	}
	// Check direct peers first
	for _, peer := range m.topology.GetAllPeers() {
		if peer.NodeID() == nodeID && peer.Subnet != nil {
			vip := DeriveVIPFromSubnet(peer.Subnet)
			if vip != nil {
				util.LogDebug("[MESH] DNS resolve: %s -> %s", domain, vip)
			}
			return vip
		}
	}
	// Check claimed subnets from all peers (for indirect nodes)
	for _, peer := range m.topology.GetAllPeers() {
		for _, cs := range peer.ClaimedSubnets {
			if cs.NodeID == nodeID && cs.Subnet != nil {
				vip := DeriveVIPFromSubnet(cs.Subnet)
				if vip != nil {
					util.LogDebug("[MESH] DNS resolve: %s -> %s (via %s)", domain, vip, peer.NodeID())
				}
				return vip
			}
		}
	}
	return nil
}

// claimHopFor returns the peer's claim hop for the given origin nodeID —
// the real distance to the origin through that peer. ok=false when the
// peer carries no claim for the nodeID (copy without path support).
func claimHopFor(peer *PeerInfo, nodeID string) (int, bool) {
	for _, cs := range peer.ClaimedSubnets {
		if cs.NodeID == nodeID {
			return cs.Hop, true
		}
	}
	return 0, false
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
		owner  string // nodeID anchoring this entry (display only)
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

	// Peer routes (non-mesh, referencing owner node). Candidate score =
	// the sender's claim hop for the owner (= real distance through that
	// peer); candidates without claim support are invalid and dropped.
	for _, peer := range peers {
		if peer.Sender == nil {
			continue
		}
		for _, r := range peer.Routes {
			hop, ok := claimHopFor(peer, r.NodeID)
			if !ok {
				continue // No claim support for the owner, drop the copy
			}
			allEntries[r.PrefixStr] = append(allEntries[r.PrefixStr], peerEntry{peer.Sender, hop, r.Prefix, r.NodeID})
		}
	}

	// Peer claimed subnets (mesh routing)
	for _, peer := range peers {
		if peer.Sender == nil {
			continue
		}
		if peer.Subnet != nil {
			key := peer.Subnet.String()
			allEntries[key] = append(allEntries[key], peerEntry{peer.Sender, 1, peer.Subnet, peer.NodeID()})
		}
		for _, cs := range peer.ClaimedSubnets {
			// The peer's own claim (hop=1, own nodeID) duplicates the
			// peer.Subnet entry already appended above.
			if cs.Hop == 1 && cs.NodeID == peer.NodeID() {
				continue
			}
			allEntries[cs.SubnetStr] = append(allEntries[cs.SubnetStr], peerEntry{peer.Sender, cs.Hop, cs.Subnet, cs.NodeID})
		}
	}

	// Build MeshRoute slice: exclude own prefixes from peer routes.
	routes := make([]MeshRoute, 0, len(allEntries)+1+len(advertise))
	for prefixStr, entries := range allEntries {
		if ownPrefixes[prefixStr] {
			continue
		}
		// Build route entries (owner nodeIDs)
		ownerSet := make(map[string]bool, len(entries))
		routeEntries := make([]RouteEntry, 0, len(entries))
		for _, e := range entries {
			if e.owner != "" && !ownerSet[e.owner] {
				ownerSet[e.owner] = true
				routeEntries = append(routeEntries, RouteEntry{NodeID: e.owner, Source: RouteSourceDynamic})
			}
		}
		routes = append(routes, MeshRoute{
			Prefix:  entries[0].prefix,
			Entries: routeEntries,
		})
	}

	// Add own subnet as local route.
	if ownSubnet != nil {
		routes = append(routes, MeshRoute{
			Prefix:  ownSubnet,
			Entries: []RouteEntry{{NodeID: m.nodeID, Source: RouteSourceDynamic}},
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
			Entries: []RouteEntry{{NodeID: m.nodeID, Source: RouteSourceDynamic}},
		})
	}

	// Merge static routes into unified route table
	m.mu.RLock()
	for _, staticRoute := range m.staticRoutes {
		_, prefix, err := net.ParseCIDR(staticRoute.Prefix)
		if err != nil {
			continue
		}

		// Build static entries
		staticEntries := make([]RouteEntry, 0, len(staticRoute.NodeIDs))
		for _, nodeID := range staticRoute.NodeIDs {
			staticEntries = append(staticEntries, RouteEntry{NodeID: nodeID, Source: RouteSourceStatic})
		}

		// Check if there's already a dynamic route with the same prefix
		found := false
		for i := range routes {
			if routes[i].Prefix.String() == prefix.String() {
				// Merge: prepend static entries (static first)
				routes[i].Entries = append(staticEntries, routes[i].Entries...)
				found = true
				break
			}
		}

		if !found {
			// Add new route with static entries only
			routes = append(routes, MeshRoute{
				Prefix:  prefix,
				Entries: staticEntries,
			})
		}
	}
	m.mu.RUnlock()

	// Sort routes by prefix length (longest first) for longest-match lookup.
	sort.Slice(routes, func(i, j int) bool {
		lenI, _ := routes[i].Prefix.Mask.Size()
		lenJ, _ := routes[j].Prefix.Mask.Size()
		return lenI > lenJ
	})

	// Build unified domain trie (static + dynamic merged)
	domainTrie := NewNodeTrie()

	// Add own domain suffixes (from config) as static
	ownSuffixSet := make(map[string]bool, len(domainSuffixes))
	for _, s := range domainSuffixes {
		domainTrie.Insert(s, m.nodeID, RouteSourceStatic) // own suffixes point to self
		ownSuffixSet[strings.ToLower(strings.TrimPrefix(s, "."))] = true
	}

	// Add static domain suffix routes (from config) as static
	m.mu.RLock()
	for _, suffixRoute := range m.staticDomainSuffixes {
		for _, nodeID := range suffixRoute.NodeIDs {
			domainTrie.Insert(suffixRoute.Suffix, nodeID, RouteSourceStatic)
		}
	}
	m.mu.RUnlock()

	// Build a map of nodeID → (subnet, hop) from ClaimedSubnets
	type nodeInfoLocal struct {
		subnet *net.IPNet
		hop    int
	}
	nodeIDToInfo := make(map[string]nodeInfoLocal)
	for _, peer := range peers {
		if peer.Sender == nil {
			continue
		}
		// Peer's own subnet
		if peer.Subnet != nil {
			if _, exists := nodeIDToInfo[peer.NodeID()]; !exists {
				nodeIDToInfo[peer.NodeID()] = nodeInfoLocal{peer.Subnet, 1}
			}
		}
		// Peer's learned claims
		for _, cs := range peer.ClaimedSubnets {
			if existing, exists := nodeIDToInfo[cs.NodeID]; !exists || cs.Hop < existing.hop {
				nodeIDToInfo[cs.NodeID] = nodeInfoLocal{cs.Subnet, cs.Hop}
			}
		}
	}

	// Add peer domain suffixes (from gossip) as dynamic — skip if we own the same suffix
	for _, peer := range peers {
		if peer.Sender == nil {
			continue
		}
		for _, entry := range peer.DomainSuffixes {
			normalized := strings.ToLower(strings.TrimPrefix(entry.Suffix, "."))
			if ownSuffixSet[normalized] {
				continue
			}
			// Insert into unified trie with dynamic source
			domainTrie.Insert(entry.Suffix, entry.NodeID, RouteSourceDynamic)
		}
	}

	// Static trie: auto-generate nodeID.phn entries from claimed subnets
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
	// Insert nodeID.phn entries into unified domain trie as static
	for nid := range bestNodes {
		domainTrie.Insert(nid+"."+MeshDomainSuffix, nid, RouteSourceStatic)
	}

	// Convert bestNodes to nodeMap for routeTable
	nodeMap := make(map[string]*nodeInfo)
	for nid, claim := range bestNodes {
		nodeMap[nid] = &nodeInfo{
			sender: claim.sender,
			subnet: claim.subnet,
			hop:    claim.hop,
		}
	}

	// Build topology graph and compute best paths using Dijkstra
	graph := m.buildTopologyGraph()
	bestPaths := dijkstra(graph, m.nodeID)

	// Atomically swap in the new route table (lock-free for readers)
	m.routeTable.Store(&routeTable{
		routes:     routes,
		domainTrie: domainTrie,
		nodeMap:    nodeMap,
		bestPaths:  bestPaths,
	})
	util.LogInfo("[MESH] routes installed: %d routes", len(routes))

	// Notify netstack of route change (for tunnel NIC updates)
	if m.onRouteChange != nil {
		m.onRouteChange()
	}

	for _, r := range routes {
		var nodeIDs []string
		for _, e := range r.Entries {
			nodeIDs = append(nodeIDs, e.NodeID)
		}
		util.LogDebug("[MESH]   %s -> %s", r.Prefix, strings.Join(nodeIDs, ", "))
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
func (m *MeshManager) findRoute(dstIP net.IP) *MeshRoute {
	rt := m.getRouteTable()

	for i := range rt.routes {
		if rt.routes[i].Prefix.Contains(dstIP) {
			return &rt.routes[i]
		}
	}
	return nil
}

// findNextHops returns the next hop peers for a given destination IP.
func (m *MeshManager) findNextHops(dstIP net.IP) []PeerWithHop {
	rt := m.getRouteTable()

	// Step 1: Find route (prefix → nodeIDs)
	var targetNodeIDs []string
	for i := range rt.routes {
		if rt.routes[i].Prefix.Contains(dstIP) {
			for _, entry := range rt.routes[i].Entries {
				targetNodeIDs = append(targetNodeIDs, entry.NodeID)
			}
			break
		}
	}
	if len(targetNodeIDs) == 0 {
		return nil
	}

	// Step 2: Use Dijkstra results to find best next hop
	peers := m.topology.GetAllPeers()

	// Find the best target based on Dijkstra path cost
	var bestNextHop string
	bestCost := 1e18
	for _, targetNodeID := range targetNodeIDs {
		if targetNodeID == m.nodeID {
			continue // skip self
		}
		if path, ok := rt.bestPaths[targetNodeID]; ok {
			if path.Cost < bestCost {
				bestCost = path.Cost
				bestNextHop = path.NextHop
			}
		}
	}

	if bestNextHop == "" {
		// Fallback: no Dijkstra path found, use old logic
		return m.findNextHopsFallback(targetNodeIDs, peers)
	}

	// Find ALL peers that connect to the best next hop node
	// (multiple links to same node = multiple peers with same NodeID)
	var result []PeerWithHop
	for _, peer := range peers {
		if peer.Sender != nil && peer.NodeID() == bestNextHop {
			result = append(result, PeerWithHop{Peer: peer.Sender, Hop: 1})
		}
	}

	return result
}

// findNextHopsFallback is the fallback logic when Dijkstra paths are not available.
// Returns ALL peers (including multiple links to same node) sorted by hop count.
// Multi-link load balancing is handled by SendToNode (design §2.8).
func (m *MeshManager) findNextHopsFallback(targetNodeIDs []string, peers []*PeerInfo) []PeerWithHop {
	type peerWithHop struct {
		peer PeerSender
		hop  int
	}
	var candidates []peerWithHop

	for _, targetNodeID := range targetNodeIDs {
		if targetNodeID == m.nodeID {
			continue
		}
		for _, peer := range peers {
			if peer.Sender == nil {
				continue
			}
			if peer.NodeID() == targetNodeID {
				candidates = append(candidates, peerWithHop{peer.Sender, 1})
				continue
			}
			for _, cs := range peer.ClaimedSubnets {
				if cs.NodeID == targetNodeID {
					candidates = append(candidates, peerWithHop{peer.Sender, cs.Hop})
					break
				}
			}
		}
	}

	if len(candidates) == 0 {
		return nil
	}

	// Sort by hop count (lowest first), then by peer ID for deterministic ordering
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].hop != candidates[j].hop {
			return candidates[i].hop < candidates[j].hop
		}
		return candidates[i].peer.GetNodeID() < candidates[j].peer.GetNodeID()
	})

	// Return ALL candidates (no deduplication) for multi-link load balancing
	result := make([]PeerWithHop, len(candidates))
	for i, c := range candidates {
		result[i] = PeerWithHop{Peer: c.peer, Hop: c.hop}
	}

	return result
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
				nodeID := ev.sender.GetNodeID()
				// Reset quality tracker for this peer to avoid stale state
				m.qualityTracker.RemoveNode(nodeID)
				util.LogInfo("[MESH] peer registered: %s", nodeID)
				// Immediately broadcast gossip so routing is established without waiting for 15s tick
				m.broadcastGossip()
				// Notify for package sync
				if m.OnPeerRegistered != nil {
					go m.OnPeerRegistered(nodeID)
				}
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
				util.LogDebug("[MESH] gossip from %s: routes=%d domainSuffixes=%d claimedSubnets=%d", ev.sender.GetNodeID(), len(info.Routes), len(info.DomainSuffixes), len(info.ClaimedSubnets))
				if m.topology.UpdateGossip(ev.sender, info) {
					m.recomputeRoutes()
					util.DefaultVersionNotifier.BumpVersion("mesh")
				}
				// Check for subnet conflicts and re-select if needed
				if m.checkSubnetConflict() {
					m.recomputeRoutes()
					// Resend hello to all peers to clean up old state and advertise new subnet/VIP
					if m.p2p != nil {
						m.p2p.ResendHelloToAll()
					}
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

	// Build global claimed subnets table (mesh subnets)
	type globalClaimEntry struct {
		hop       int
		nextHop   *PeerInfo // nil = own claim
		nodeID    string
		subnet    string
		neighbors []GossipNeighbor
	}
	bestClaims := make(map[string]globalClaimEntry)

	// Collect own neighbors (direct peer nodeIDs with link quality)
	// Group links by nodeID (support multiple links between same nodes)
	neighborMap := make(map[string]*GossipNeighbor)
	for _, peer := range allPeers {
		if peer.Sender != nil {
			nodeID := peer.NodeID()
			if _, ok := neighborMap[nodeID]; !ok {
				neighborMap[nodeID] = &GossipNeighbor{
					NodeID: nodeID,
					Links:  []GossipLink{},
				}
			}
			// Get seq values for gossip advertisement
			localSeq := peer.Sender.GetLocalSeq()
			remoteSeq := peer.Sender.GetRemoteSeq()
			friendlyName := peer.Sender.GetFriendlyName() // empty for passive peers

			link := GossipLink{
				LocalSeq:     localSeq,
				RemoteSeq:    remoteSeq,
				FriendlyName: friendlyName,
			}

			// Use localSeq as internal key for qualityTracker
			if m.qualityTracker != nil {
				quality := m.qualityTracker.Get(nodeID, localSeq)
				avgRTT, lossRate, jitter := quality.Stats()
				link.SRTT = float64(avgRTT.Milliseconds())
				link.LossRate = lossRate
				link.Jitter = jitter
			}
			neighborMap[nodeID].Links = append(neighborMap[nodeID].Links, link)
		}
	}

	// Convert map to slice
	ownNeighbors := make([]GossipNeighbor, 0, len(neighborMap))
	for _, neighbor := range neighborMap {
		ownNeighbors = append(ownNeighbors, *neighbor)
	}

	// Own claim (Hop=0, with neighbors)
	bestClaims[m.subnetStr] = globalClaimEntry{0, nil, m.nodeID, m.subnetStr, ownNeighbors}

	// Peer claims
	for _, peer := range allPeers {
		if peer.Sender == nil {
			continue
		}
		// Peer's learned claims (includes peer's own subnet with hop=1)
		for _, cs := range peer.ClaimedSubnets {
			if existing, ok := bestClaims[cs.SubnetStr]; !ok || cs.Hop < existing.hop {
				bestClaims[cs.SubnetStr] = globalClaimEntry{cs.Hop, peer, cs.NodeID, cs.SubnetStr, cs.Neighbors}
			}
		}
	}

	// Mutual neighbor validation: filter out claims where origin's neighbors
	// don't reciprocally declare the origin.
	// Rule: if X declares neighbors=[Y], then Y must also declare X.
	// This prevents stale claims from wandering after a node goes offline.
	nodeIDToClaim := make(map[string]globalClaimEntry, len(bestClaims))
	for _, c := range bestClaims {
		nodeIDToClaim[c.nodeID] = c
	}
	validatedClaims := make(map[string]globalClaimEntry, len(bestClaims))
	for subnet, claim := range bestClaims {
		if claim.hop == 0 {
			// Own claim, always valid
			validatedClaims[subnet] = claim
			continue
		}
		valid := true
		for _, neighbor := range claim.neighbors {
			neighborClaim, ok := nodeIDToClaim[neighbor.NodeID]
			if !ok {
				valid = false
				break
			}
			if !containsNeighborNodeID(neighborClaim.neighbors, claim.nodeID) {
				valid = false
				break
			}
		}
		if valid {
			validatedClaims[subnet] = claim
		}
	}
	bestClaims = validatedClaims

	// Build global route table (non-mesh routes, referencing owner node)
	type globalRouteEntry struct {
		nextHop *PeerInfo // nil = own route
		nodeID  string
		prefix  string
		hop     int // real distance to the owner through nextHop (0 = own route)
	}
	bestRoutes := make(map[string]globalRouteEntry)

	// Own advertise routes
	for _, r := range advertise {
		_, ipNet, _ := net.ParseCIDR(r)
		bestRoutes[r] = globalRouteEntry{nil, m.nodeID, r, 0}
		_ = ipNet // ipNet not used, kept for potential future use
	}
	// Peer routes (reference owner node). Score = the sender's claim hop
	// for the owner (real distance through that peer); copies without
	// claim support are invalid and dropped.
	for _, peer := range allPeers {
		if peer.Sender == nil {
			continue
		}
		for _, r := range peer.Routes {
			hop, ok := claimHopFor(peer, r.NodeID)
			if !ok {
				continue
			}
			if e, exists := bestRoutes[r.PrefixStr]; !exists || hop < e.hop {
				bestRoutes[r.PrefixStr] = globalRouteEntry{peer, r.NodeID, r.PrefixStr, hop}
			}
		}
	}

	// Build global domain suffix map (referencing owner node)
	type globalDSEntry struct {
		nextHop *PeerInfo // nil = own entry
		nodeID  string
		hop     int // real distance to the owner through nextHop (0 = own entry)
	}
	bestDS := make(map[string]globalDSEntry)
	for _, s := range domainSuffixes {
		bestDS[s] = globalDSEntry{nil, m.nodeID, 0}
	}
	for _, peer := range allPeers {
		if peer.Sender == nil {
			continue
		}
		for _, entry := range peer.DomainSuffixes {
			hop, ok := claimHopFor(peer, entry.NodeID)
			if !ok {
				continue
			}
			if e, exists := bestDS[entry.Suffix]; !exists || hop < e.hop {
				bestDS[entry.Suffix] = globalDSEntry{peer, entry.NodeID, hop}
			}
		}
	}

	// Per-peer: filter by split horizon and send
	for _, peer := range allPeers {
		if peer.Sender == nil {
			continue
		}

		// Filter routes: exclude entries where nextHop's nodeID == this peer's nodeID
		var routes []GossipRoute
		for _, e := range bestRoutes {
			if e.nextHop != nil && e.nextHop.NodeID() == peer.NodeID() {
				continue // split horizon
			}
			routes = append(routes, GossipRoute{Prefix: e.prefix, NodeID: e.nodeID})
		}

		// Filter domain suffixes: exclude entries where nextHop's nodeID == this peer's nodeID
		var ds []GossipDomainSuffix
		for suffix, e := range bestDS {
			if e.nextHop != nil && e.nextHop.NodeID() == peer.NodeID() {
				continue // split horizon
			}
			ds = append(ds, GossipDomainSuffix{Suffix: suffix, NodeID: e.nodeID})
		}

		// Filter claimed subnets: exclude entries where nextHop's nodeID == this peer's nodeID
		var claims []GossipClaimedSubnet
		for _, e := range bestClaims {
			if e.nextHop != nil && e.nextHop.NodeID() == peer.NodeID() {
				continue // split horizon
			}
			claims = append(claims, GossipClaimedSubnet{
				Subnet:    e.subnet,
				NodeID:    e.nodeID,
				Hop:       e.hop,
				Neighbors: e.neighbors,
			})
		}

		info := GossipInfo{
			Cmd:            "gossip",
			DomainSuffixes: ds,
			Routes:         routes,
			ClaimedSubnets: claims,
		}
		data, err := json.Marshal(info)
		if err != nil {
			continue
		}
		util.LogInfo("[MESH] sending gossip to %s: claimedSubnets=%d routes=%d", peer.NodeID(), len(claims), len(routes))
		peer.Sender.SendGossip(data)
	}
}

// qualityLoop periodically pulls ACK-based link quality stats from P2P layer
// and checks for peer connectivity issues.
func (m *MeshManager) qualityLoop() {
	util.LogInfo("[MESH] quality loop started (interval=10s)")
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.closeCh:
			return
		case <-ticker.C:
			m.updateACKStats()
			m.checkPeerConnectivity()
		}
	}
}

// checkPeerConnectivity disconnects helloed links whose ACK updates have stalled.
func (m *MeshManager) checkPeerConnectivity() {
	if m.p2p == nil {
		return
	}

	allPeers := m.topology.GetAllPeers()
	for _, peer := range allPeers {
		if peer.Sender == nil {
			continue // not a direct peer
		}

		nodeID := peer.NodeID()
		localSeq := peer.Sender.GetLocalSeq()
		linkID := peer.Sender.GetLinkID()
		if linkID == "" {
			continue
		}
		quality := m.qualityTracker.Get(nodeID, localSeq)

		// Disconnect only after ACK updates stall.
		if time.Since(quality.LastACKUpdate()) > 60*time.Second {
			// A sampled link that stops advancing is stale.
			_, _, _, _, sampleCount := m.p2p.GetLinkQualityStatsByLinkID(linkID)
			if sampleCount > 0 {
				// The control plane stopped progressing for this link.
				util.LogWarn("[MESH] peer %s (link %s) ACK updates stalled for 60s, disconnecting", nodeID, linkID)
				m.p2p.StopPeerByLinkID(linkID)
				quality.Reset()
			}
		}
	}
}

// updateACKStats pulls ACK-based link quality stats from P2P layer (v7).
// This provides passive RTT and loss measurement without extra probe traffic.
func (m *MeshManager) updateACKStats() {
	if m.p2p == nil {
		return
	}

	allPeers := m.topology.GetAllPeers()
	util.LogInfo("[MESH-ACK] updateACKStats: %d peers", len(allPeers))
	for _, peer := range allPeers {
		if peer.Sender == nil {
			continue // not a direct peer
		}

		nodeID := peer.NodeID()
		proxyName := peer.Sender.GetProxyName()
		srtt, _, jitter, lossRate, sampleCount := m.p2p.GetLinkQualityStatsByProxy(proxyName)
		util.LogInfo("[MESH-ACK] peer=%s proxy=%s srtt=%v jitter=%v lossRate=%.4f samples=%d", nodeID, proxyName, srtt, jitter, lossRate, sampleCount)
		if srtt > 0 || lossRate > 0 {
			// Use localSeq as key
			localSeq := peer.Sender.GetLocalSeq()
			quality := m.qualityTracker.Get(nodeID, localSeq)
			quality.UpdateACKStats(srtt, lossRate, jitter, sampleCount)
		}
	}
}

// GetPeerQuality returns the quality metrics for a peer (first link found).
// Deprecated: Use GetLinkQuality for per-link tracking.
func (m *MeshManager) GetPeerQuality(nodeID string) (avgRTT time.Duration, loss, jitter float64) {
	quality := m.qualityTracker.GetByNode(nodeID)
	if quality == nil {
		return 0, 0, 0
	}
	return quality.Stats()
}

// GetLinkQuality returns the quality metrics for a specific link.
func (m *MeshManager) GetLinkQuality(nodeID string, localSeq uint16) (avgRTT time.Duration, loss, jitter float64) {
	quality := m.qualityTracker.Get(nodeID, localSeq)
	return quality.Stats()
}

// GetLinkNICStats returns injection statistics for all LinkNICs.
func (m *MeshManager) GetLinkNICStats() map[string]struct {
	Packets uint64
	Bytes   uint64
	Drops   uint64
} {
	if m.tun == nil || m.tun.GetNetstack() == nil {
		return nil
	}
	return m.tun.GetNetstack().LinkNICStats()
}

// selectBestPeer selects the best peer from candidates.
// Priority: lowest hop count first, then best link quality (lowest effectiveRTT) as tiebreaker.
// Falls back to random selection among min-hop candidates if no quality data.
func (m *MeshManager) selectBestPeer(candidates []PeerWithHop, dstIP net.IP) PeerSender {
	peers := m.selectBestPeers(candidates, dstIP)
	if len(peers) == 0 {
		return nil
	}
	return peers[0]
}

// selectBestPeers returns candidates sorted by preference (best first).
// Priority: lowest hop count first, then best link quality (lowest effectiveRTT) as tiebreaker.
func (m *MeshManager) selectBestPeers(candidates []PeerWithHop, dstIP net.IP) []PeerSender {
	if len(candidates) == 0 {
		return nil
	}

	// Find minimum hop count
	minHop := candidates[0].Hop
	for _, c := range candidates {
		if c.Hop < minHop {
			minHop = c.Hop
		}
	}

	// Filter candidates with min hop count
	var minHopCandidates []PeerWithHop
	for _, c := range candidates {
		if c.Hop == minHop {
			minHopCandidates = append(minHopCandidates, c)
		}
	}

	// Among min-hop candidates, sort by link quality (lowest effectiveRTT first)
	type peerQuality struct {
		peer        PeerSender
		effectiveRT time.Duration
		hasData     bool
	}

	qualities := make([]peerQuality, len(minHopCandidates))
	anyData := false
	for i, c := range minHopCandidates {
		nodeID := c.Peer.GetNodeID()
		avgRTT, loss, jitter := m.GetPeerQuality(nodeID)
		hasData := avgRTT > 0
		if hasData {
			anyData = true
			// Cost = SRTT * (1 + lossRate) + jitter
			effectiveRT := time.Duration(float64(avgRTT)*(1.0+loss) + jitter)
			qualities[i] = peerQuality{peer: c.Peer, effectiveRT: effectiveRT, hasData: hasData}
		} else {
			qualities[i] = peerQuality{peer: c.Peer, hasData: false}
		}
	}

	// If no quality data, shuffle min-hop candidates for load balancing
	if !anyData {
		result := make([]PeerSender, len(minHopCandidates))
		perm := rand.Perm(len(minHopCandidates))
		for i, idx := range perm {
			result[i] = minHopCandidates[idx].Peer
		}
		return result
	}

	// Sort: peers with data first (by effectiveRTT), then peers without data
	var withData, withoutData []PeerSender
	for _, pq := range qualities {
		if pq.hasData {
			withData = append(withData, pq.peer)
		} else {
			withoutData = append(withoutData, pq.peer)
		}
	}

	// Sort withData by effectiveRTT (bubble sort for simplicity, list is small)
	for i := 0; i < len(withData); i++ {
		for j := i + 1; j < len(withData); j++ {
			_, lossI, jitterI := m.GetPeerQuality(withData[i].GetNodeID())
			_, lossJ, jitterJ := m.GetPeerQuality(withData[j].GetNodeID())
			avgI, _, _ := m.GetPeerQuality(withData[i].GetNodeID())
			avgJ, _, _ := m.GetPeerQuality(withData[j].GetNodeID())
			effectiveI := time.Duration(float64(avgI)*(1.0+lossI) + jitterI)
			effectiveJ := time.Duration(float64(avgJ)*(1.0+lossJ) + jitterJ)
			if effectiveI > effectiveJ {
				withData[i], withData[j] = withData[j], withData[i]
			}
		}
	}

	// Shuffle withoutData for load balancing
	rand.Shuffle(len(withoutData), func(i, j int) {
		withoutData[i], withoutData[j] = withoutData[j], withoutData[i]
	})

	return append(withData, withoutData...)
}

// getHopForPeer returns the hop count for a selected peer from the candidates list.
func (m *MeshManager) getHopForPeer(candidates []PeerWithHop, peer PeerSender) int {
	for _, c := range candidates {
		if c.Peer == peer {
			return c.Hop
		}
	}
	return 0
}

// BuildGossipInfo builds the current gossip content (used for hello messages).
// Returns a global view without per-peer split horizon filtering.
func (m *MeshManager) BuildGossipInfo() *GossipInfo {
	allPeers := m.topology.GetAllPeers()

	m.mu.RLock()
	advertise := make([]string, len(m.advertise))
	copy(advertise, m.advertise)
	domainSuffixes := make([]string, len(m.domainSuffixes))
	copy(domainSuffixes, m.domainSuffixes)
	m.mu.RUnlock()

	// Build global claimed subnets table (mesh subnets)
	type globalClaimEntry struct {
		hop       int
		nodeID    string
		subnet    string
		neighbors []GossipNeighbor
	}
	bestClaims := make(map[string]globalClaimEntry)

	// Collect own neighbors (direct peer nodeIDs with link quality)
	// Group links by nodeID (support multiple links between same nodes)
	neighborMap := make(map[string]*GossipNeighbor)
	for _, peer := range allPeers {
		if peer.Sender != nil {
			nodeID := peer.NodeID()
			if _, ok := neighborMap[nodeID]; !ok {
				neighborMap[nodeID] = &GossipNeighbor{
					NodeID: nodeID,
					Links:  []GossipLink{},
				}
			}
			// Get seq values for gossip advertisement
			localSeq := peer.Sender.GetLocalSeq()
			remoteSeq := peer.Sender.GetRemoteSeq()
			friendlyName := peer.Sender.GetFriendlyName() // empty for passive peers

			link := GossipLink{
				LocalSeq:     localSeq,
				RemoteSeq:    remoteSeq,
				FriendlyName: friendlyName,
			}

			// Use localSeq as internal key for qualityTracker
			if m.qualityTracker != nil {
				quality := m.qualityTracker.Get(nodeID, localSeq)
				avgRTT, lossRate, jitter := quality.Stats()
				link.SRTT = float64(avgRTT.Milliseconds())
				link.LossRate = lossRate
				link.Jitter = jitter
			}
			neighborMap[nodeID].Links = append(neighborMap[nodeID].Links, link)
		}
	}

	// Convert map to slice
	ownNeighbors := make([]GossipNeighbor, 0, len(neighborMap))
	for _, neighbor := range neighborMap {
		ownNeighbors = append(ownNeighbors, *neighbor)
	}

	// Own claim (Hop=0, with neighbors)
	bestClaims[m.subnetStr] = globalClaimEntry{0, m.nodeID, m.subnetStr, ownNeighbors}

	// Peer claims
	for _, peer := range allPeers {
		if peer.Sender == nil {
			continue
		}
		for _, cs := range peer.ClaimedSubnets {
			if existing, ok := bestClaims[cs.SubnetStr]; !ok || cs.Hop < existing.hop {
				bestClaims[cs.SubnetStr] = globalClaimEntry{cs.Hop, cs.NodeID, cs.SubnetStr, cs.Neighbors}
			}
		}
	}

	// Build global route table (non-mesh routes, referencing owner node).
	// Score = the sender's claim hop for the owner (real distance through
	// that peer); copies without claim support are invalid and dropped.
	type routeEntry struct {
		route GossipRoute
		hop   int // 0 = own route
	}
	bestRoutes := make(map[string]routeEntry)

	// Own advertise routes
	for _, r := range advertise {
		bestRoutes[r] = routeEntry{GossipRoute{Prefix: r, NodeID: m.nodeID}, 0}
	}
	// Peer routes (reference owner node)
	for _, peer := range allPeers {
		if peer.Sender == nil {
			continue
		}
		for _, r := range peer.Routes {
			hop, ok := claimHopFor(peer, r.NodeID)
			if !ok {
				continue
			}
			if e, exists := bestRoutes[r.PrefixStr]; !exists || hop < e.hop {
				bestRoutes[r.PrefixStr] = routeEntry{GossipRoute{Prefix: r.PrefixStr, NodeID: r.NodeID}, hop}
			}
		}
	}

	// Build global domain suffix map (referencing owner node)
	type dsEntry struct {
		ds  GossipDomainSuffix
		hop int // 0 = own entry
	}
	bestDS := make(map[string]dsEntry)
	for _, s := range domainSuffixes {
		bestDS[s] = dsEntry{GossipDomainSuffix{Suffix: s, NodeID: m.nodeID}, 0}
	}
	for _, peer := range allPeers {
		if peer.Sender == nil {
			continue
		}
		for _, entry := range peer.DomainSuffixes {
			hop, ok := claimHopFor(peer, entry.NodeID)
			if !ok {
				continue
			}
			if e, exists := bestDS[entry.Suffix]; !exists || hop < e.hop {
				bestDS[entry.Suffix] = dsEntry{GossipDomainSuffix{Suffix: entry.Suffix, NodeID: entry.NodeID}, hop}
			}
		}
	}

	// Convert to slices
	routes := make([]GossipRoute, 0, len(bestRoutes))
	for _, e := range bestRoutes {
		routes = append(routes, e.route)
	}

	ds := make([]GossipDomainSuffix, 0, len(bestDS))
	for _, e := range bestDS {
		ds = append(ds, e.ds)
	}

	claims := make([]GossipClaimedSubnet, 0, len(bestClaims))
	for _, c := range bestClaims {
		claims = append(claims, GossipClaimedSubnet{
			Subnet:    c.subnet,
			NodeID:    c.nodeID,
			Hop:       c.hop,
			Neighbors: c.neighbors,
		})
	}

	return &GossipInfo{
		DomainSuffixes: ds,
		Routes:         routes,
		ClaimedSubnets: claims,
	}
}

// LinkQualityInfo represents a link in the topology graph.
type LinkQualityInfo struct {
	From      string  // source node ID
	To        string  // target node ID
	LocalSeq  uint16  // local sequence number
	RemoteSeq uint16  // remote sequence number
	SRTT      float64 // smoothed RTT in milliseconds
	LossRate  float64 // loss rate (0.0-1.0)
	Jitter    float64 // jitter in milliseconds
	Cost      float64 // path cost = SRTT * (1 + LossRate) + Jitter
}

// TopologyGraph represents the global topology with link qualities.
type TopologyGraph struct {
	Nodes map[string]bool                          // all nodes
	Edges map[string]map[string][]*LinkQualityInfo // from -> to -> links (multiple links supported)
}

// buildTopologyGraph builds a global topology graph from all received gossip.
func (m *MeshManager) buildTopologyGraph() *TopologyGraph {
	graph := &TopologyGraph{
		Nodes: make(map[string]bool),
		Edges: make(map[string]map[string][]*LinkQualityInfo),
	}

	// Add own links
	selfID := m.nodeID
	graph.Nodes[selfID] = true
	allPeers := m.topology.GetAllPeers()
	for _, peer := range allPeers {
		if peer.Sender == nil {
			continue
		}
		neighborID := peer.NodeID()
		graph.Nodes[neighborID] = true

		localSeq := peer.Sender.GetLocalSeq()
		remoteSeq := peer.Sender.GetRemoteSeq()
		link := &LinkQualityInfo{
			From:      selfID,
			To:        neighborID,
			LocalSeq:  localSeq,
			RemoteSeq: remoteSeq,
		}
		if m.qualityTracker != nil {
			quality := m.qualityTracker.Get(neighborID, localSeq)
			avgRTT, lossRate, jitter := quality.Stats()
			link.SRTT = float64(avgRTT.Milliseconds())
			link.LossRate = lossRate
			link.Jitter = jitter
			// Cost = SRTT * (1 + lossRate) + jitter
			link.Cost = link.SRTT*(1+lossRate) + jitter
		} else {
			link.Cost = 1000
		}

		if graph.Edges[selfID] == nil {
			graph.Edges[selfID] = make(map[string][]*LinkQualityInfo)
		}
		// Append link (support multiple links)
		graph.Edges[selfID][neighborID] = append(graph.Edges[selfID][neighborID], link)
	}

	// Extract links from other nodes' gossip
	for _, peer := range allPeers {
		if peer.Sender == nil {
			continue
		}
		for _, claim := range peer.ClaimedSubnets {
			// Only process the peer's own subnet claim (hop=1 after increment)
			if claim.NodeID == peer.NodeID() && claim.Hop == 1 {
				fromNode := peer.NodeID()
				graph.Nodes[fromNode] = true

				for _, neighbor := range claim.Neighbors {
					toNode := neighbor.NodeID
					graph.Nodes[toNode] = true

					// Process all links to this neighbor
					for _, gossipLink := range neighbor.Links {
						link := &LinkQualityInfo{
							From:      fromNode,
							To:        toNode,
							LocalSeq:  gossipLink.LocalSeq,
							RemoteSeq: gossipLink.RemoteSeq,
							SRTT:      gossipLink.SRTT,
							LossRate:  gossipLink.LossRate,
						}
						if gossipLink.SRTT > 0 && gossipLink.LossRate < 1.0 {
							link.Cost = gossipLink.SRTT / (1 - gossipLink.LossRate)
						} else {
							link.Cost = 1000 // default high cost
						}

						if graph.Edges[fromNode] == nil {
							graph.Edges[fromNode] = make(map[string][]*LinkQualityInfo)
						}
						// Append link (support multiple links)
						graph.Edges[fromNode][toNode] = append(graph.Edges[fromNode][toNode], link)
					}
				}
			}
		}
	}

	return graph
}

// dijkstraResult represents the result of Dijkstra's algorithm for a single destination.
type dijkstraResult struct {
	NextHop  string  // next hop node ID
	LocalSeq uint16  // localSeq of the best link to next hop
	Cost     float64 // total path cost
}

// dijkstra computes shortest paths from source to all other nodes.
// Returns a map: nodeID -> dijkstraResult.
func dijkstra(graph *TopologyGraph, source string) map[string]dijkstraResult {
	type nodeInfo struct {
		cost     float64
		nextHop  string
		localSeq uint16 // localSeq of the best link to next hop
		visited  bool
	}

	nodes := make(map[string]*nodeInfo)
	for nodeID := range graph.Nodes {
		nodes[nodeID] = &nodeInfo{
			cost: 1e18, // infinity
		}
	}
	nodes[source].cost = 0

	for {
		// Find unvisited node with minimum cost
		var minNode string
		minCost := 1e18
		for nodeID, info := range nodes {
			if !info.visited && info.cost < minCost {
				minNode = nodeID
				minCost = info.cost
			}
		}

		if minNode == "" || minCost >= 1e18 {
			break // all reachable nodes visited
		}

		nodes[minNode].visited = true

		// Update neighbors' costs
		if neighbors, ok := graph.Edges[minNode]; ok {
			for toNode, links := range neighbors {
				if nodes[toNode].visited {
					continue
				}
				// Find the best link (lowest cost, then smallest localSeq for deterministic tie-breaking) among multiple links
				var bestCost float64 = 1e18
				var bestLocalSeq uint16
				for _, link := range links {
					if link.Cost < bestCost || (link.Cost == bestCost && link.LocalSeq < bestLocalSeq) {
						bestCost = link.Cost
						bestLocalSeq = link.LocalSeq
					}
				}
				newCost := nodes[minNode].cost + bestCost

				// Determine next hop
				var nextHop string
				var localSeq uint16
				if minNode == source {
					nextHop = toNode // direct neighbor
					localSeq = bestLocalSeq
				} else {
					nextHop = nodes[minNode].nextHop   // inherit next hop
					localSeq = nodes[minNode].localSeq // inherit localSeq
				}

				// Update if better cost, or same cost with lexicographically smaller nextHop (deterministic tie-breaking)
				if newCost < nodes[toNode].cost || (newCost == nodes[toNode].cost && nextHop < nodes[toNode].nextHop) {
					nodes[toNode].cost = newCost
					nodes[toNode].nextHop = nextHop
					nodes[toNode].localSeq = localSeq
				}
			}
		}
	}

	// Build result
	result := make(map[string]dijkstraResult)
	for nodeID, info := range nodes {
		if nodeID != source && info.cost < 1e18 {
			result[nodeID] = dijkstraResult{
				NextHop:  info.nextHop,
				LocalSeq: info.localSeq,
				Cost:     info.cost,
			}
		}
	}

	return result
}

// edgeKey returns a canonical key for an edge between two nodes (sorted order).
func edgeKey(from, to string) string {
	return from + "|" + to
}

// containsStr checks if a string slice contains a specific string.
func containsStr(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}

// containsNeighborNodeID checks if a GossipNeighbor slice contains a specific node ID.
func containsNeighborNodeID(slice []GossipNeighbor, nodeID string) bool {
	for _, item := range slice {
		if item.NodeID == nodeID {
			return true
		}
	}
	return false
}
