package mesh

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"phaethon/util"
)

const (
	// MeshDomainSuffix is the special domain suffix for mesh nodes
	MeshDomainSuffix = ".phn"
)

// NodeDomain returns the domain name for a node: <nodeid>.mesh.local
func NodeDomain(nodeID string) string {
	return nodeID + MeshDomainSuffix
}

// ParseNodeDomain extracts nodeID from a mesh domain, returns empty if not a mesh domain
func ParseNodeDomain(domain string) string {
	domain = strings.ToLower(domain)
	if !strings.HasSuffix(domain, MeshDomainSuffix) {
		return ""
	}
	return strings.TrimSuffix(domain, MeshDomainSuffix)
}

// DeriveVIPFromNodeID generates a deterministic VIP from nodeID using hash
// The VIP is within the 100.64.0.0/16 subnet
func DeriveVIPFromNodeID(nodeID string, subnet *net.IPNet) net.IP {
	if subnet == nil {
		return nil
	}
	
	// Hash the nodeID
	hash := sha256.Sum256([]byte(nodeID))
	
	// Extract first 4 bytes as uint32
	hashVal := binary.BigEndian.Uint32(hash[:4])
	
	// Get subnet base and mask
	baseIP := subnet.IP.To4()
	if baseIP == nil {
		return nil
	}
	mask := subnet.Mask
	
	// Calculate the number of host bits
	ones, bits := mask.Size()
	hostBits := bits - ones
	if hostBits < 32 {
		// Mask the hash to fit within the subnet
		hostMask := uint32((1 << hostBits) - 1)
		hostPart := hashVal & hostMask
		
		// Combine base IP with host part
		baseVal := binary.BigEndian.Uint32(baseIP)
		vipVal := baseVal | hostPart
		
		vip := make(net.IP, 4)
		binary.BigEndian.PutUint32(vip, vipVal)
		return vip
	}
	
	return nil
}

// TunInterface abstracts the TUN engine for mesh packet injection.
type TunInterface interface {
	InjectMeshPacket(data []byte) error
	WriteMeshPacket(data []byte) error
}

// P2PTransport abstracts the P2P layer for mesh packet delivery.
type P2PTransport interface {
	SendMeshPacket(peerNodeID string, data []byte) error
	SendMeshPacketByVIP(peerVIP net.IP, data []byte) error
	BroadcastMeshGossip(data []byte) error
	ListMeshPeerIDs() []string
	SendMeshDNSQuery(peerNodeID string, domain string, queryID uint16) error
}

// MeshPeerInfo describes a connected mesh peer.
type MeshPeerInfo struct {
	NodeID   string    `json:"nodeId"`
	VIP      string    `json:"vip"`
	Direct   bool      `json:"direct"`
	LastSeen time.Time `json:"lastSeen"`
}

// PrefixRoute represents a route to a network prefix.
type PrefixRoute struct {
	Prefix  *net.IPNet // Network prefix (e.g., 100.64.1.3/32 or 0.0.0.0/0)
	NextHop net.IP     // Next hop VIP
	Cost    int        // Total cost (link cost + route cost)
}

// MeshManager coordinates mesh overlay networking.
type MeshManager struct {
	mu             sync.RWMutex
	nodeID         string
	vip            net.IP
	localVIPs      map[string]bool // all local VIPs (primary + additional) as string keys
	subnet         *net.IPNet
	subnetStr      string   // subnet CIDR string (e.g., "100.64.0.0/20")
	domainSuffixes []string // domain suffixes this node can resolve
	advertise      []string // prefixes this node advertises (e.g., ["0.0.0.0/0", "192.168.1.0/24"])
	topology       *Topology
	tun            TunInterface
	p2p            P2PTransport

	prefixRoutesMu sync.RWMutex
	prefixRoutes   []PrefixRoute // Sorted by prefix length (longest first)
	domainTrie     *DomainTrie   // Cached domain suffix trie

	dnsPendingMu sync.Mutex
	dnsPending   map[uint16]chan dnsResult // pending DNS queries by queryID
	dnsQueryID   uint16                    // rotating query ID counter

	// DNSAllocator allocates a fakeIP for a domain when this node acts as gateway.
	// Returns the allocated fakeIP or error.
	DNSAllocator func(domain string) (net.IP, error)

	// NAT table for bypass gateway mode (optional, nil if not acting as bypass gateway)
	natTable *NATTable

	closeCh chan struct{}
}

type dnsResult struct {
	fakeIP net.IP
	err    error
}

// NewMeshManager creates a new mesh manager.
func NewMeshManager(nodeID string, vip net.IP, additionalVIPs []net.IP, subnet *net.IPNet, subnetStr string, domainSuffixes []string, advertise []string) *MeshManager {
	localVIPs := map[string]bool{
		vip.To4().String(): true,
	}
	for _, v := range additionalVIPs {
		if v4 := v.To4(); v4 != nil {
			localVIPs[v4.String()] = true
		}
	}
	return &MeshManager{
		nodeID:         nodeID,
		vip:            vip.To4(),
		localVIPs:      localVIPs,
		subnet:         subnet,
		subnetStr:      subnetStr,
		domainSuffixes: domainSuffixes,
		advertise:      advertise,
		topology:       NewTopology(),
		prefixRoutes:   make([]PrefixRoute, 0),
		dnsPending:     make(map[uint16]chan dnsResult),
		closeCh:        make(chan struct{}),
	}
}

// GetVIP returns this node's VIP.
func (m *MeshManager) GetVIP() net.IP {
	return m.vip
}

// GetAllVIPs returns all local VIPs (primary + additional).
func (m *MeshManager) GetAllVIPs() []net.IP {
	result := make([]net.IP, 0, len(m.localVIPs))
	for s := range m.localVIPs {
		result = append(result, net.ParseIP(s))
	}
	return result
}

// GetSubnet returns this node's subnet string (e.g., "100.64.0.0/20").
func (m *MeshManager) GetSubnet() string {
	return m.subnetStr
}

// EnableNAT enables source NAT for bypass gateway mode.
// Outbound packets from non-mesh sources will have their srcIP rewritten to VIP.
func (m *MeshManager) EnableNAT() {
	m.natTable = NewNATTable(m.vip)
	util.LogInfo("[MESH] NAT enabled (vip=%s)", m.vip)
}

// GetNATStats returns the number of active NAT entries.
func (m *MeshManager) GetNATStats() int {
	if m.natTable == nil {
		return 0
	}
	return m.natTable.Stats()
}

// GetDomainSuffixes returns this node's domain suffixes.
func (m *MeshManager) GetDomainSuffixes() []string {
	return m.domainSuffixes
}

// TopologyRef returns the topology for external queries.
func (m *MeshManager) TopologyRef() *Topology {
	return m.topology
}

// isLocalVIP checks if the given IP is any of this node's local VIPs.
func (m *MeshManager) isLocalVIP(ip net.IP) bool {
	return m.localVIPs[ip.To4().String()]
}

// SetTun sets the TUN interface reference early, before Start() is called.
// This prevents a race where P2P receives mesh frames before Start() runs.
func (m *MeshManager) SetTun(tun TunInterface) {
	m.tun = tun
}

// GlobalMeshManager is the package-level mesh manager, set from main().
var GlobalMeshManager *MeshManager

// Start begins mesh operations.
func (m *MeshManager) Start(tun TunInterface, p2p P2PTransport) {
	m.tun = tun
	m.p2p = p2p

	m.topology.EnsureNode(m.nodeID, m.vip.String())
	m.recomputeRoutes()

	go m.gossipLoop()
	util.LogInfo("[MESH] started: nodeID=%s vip=%s", m.nodeID, m.vip)
}

// Stop halts mesh operations.
func (m *MeshManager) Stop() {
	close(m.closeCh)
}

// HandleMeshDNSQuery handles a DNS query from a remote mesh node.
// This node acts as the gateway, allocating a fakeIP from its subnet.
func (m *MeshManager) HandleMeshDNSQuery(fromNodeID string, domain string, queryID uint16) (net.IP, error) {
	util.LogInfo("[MESH] DNS query from %s: %s (queryID=%d)", fromNodeID, domain, queryID)
	if m.DNSAllocator == nil {
		return nil, fmt.Errorf("mesh DNS: no allocator configured")
	}
	fakeIP, err := m.DNSAllocator(domain)
	if err != nil {
		return nil, fmt.Errorf("mesh DNS alloc: %w", err)
	}
	util.LogInfo("[MESH] DNS allocated: %s -> %s for %s", domain, fakeIP, fromNodeID)
	return fakeIP, nil
}

// ForwardDNSQuery sends a DNS query to a remote gateway and waits for the response.
// Returns the fakeIP allocated by the gateway.
func (m *MeshManager) ForwardDNSQuery(domain string, gatewayNodeID string) (net.IP, error) {
	if m.p2p == nil {
		return nil, fmt.Errorf("mesh DNS: P2P not available")
	}

	m.dnsPendingMu.Lock()
	m.dnsQueryID++
	queryID := m.dnsQueryID
	ch := make(chan dnsResult, 1)
	m.dnsPending[queryID] = ch
	m.dnsPendingMu.Unlock()

	defer func() {
		m.dnsPendingMu.Lock()
		delete(m.dnsPending, queryID)
		m.dnsPendingMu.Unlock()
	}()

	if err := m.p2p.SendMeshDNSQuery(gatewayNodeID, domain, queryID); err != nil {
		return nil, err
	}

	select {
	case result := <-ch:
		return result.fakeIP, result.err
	case <-time.After(5 * time.Second):
		return nil, fmt.Errorf("mesh DNS: timeout waiting for response from %s", gatewayNodeID)
	case <-m.closeCh:
		return nil, fmt.Errorf("mesh DNS: stopped")
	}
}

// HandleDNSResponse delivers a DNS response from a remote gateway.
func (m *MeshManager) HandleDNSResponse(queryID uint16, domain string, fakeIP net.IP, err error) {
	m.dnsPendingMu.Lock()
	ch, ok := m.dnsPending[queryID]
	m.dnsPendingMu.Unlock()

	if ok {
		ch <- dnsResult{fakeIP: fakeIP, err: err}
	} else {
		util.LogWarn("[MESH] DNS response for unknown queryID=%d domain=%s", queryID, domain)
	}
}

// MeshDNSForwarder is the callback for DNSHijacker.MeshDNSForwarder.
// It checks the domain trie and forwards to the appropriate gateway.
// Returns the fakeIP allocated by the gateway, or (nil, nil) if no match.
func (m *MeshManager) MeshDNSForwarder(domain string) (net.IP, error) {
	gatewayNodeID, nextHop, _ := m.FindGatewayForDomain(domain)
	if gatewayNodeID == "" || nextHop == nil {
		return nil, nil // no gateway match, use local pool
	}
	if gatewayNodeID == m.nodeID {
		return nil, nil // we are the gateway, use local pool
	}

	util.LogInfo("[MESH] DNS forward: %s -> gateway %s (nextHop=%s)", domain, gatewayNodeID, nextHop)
	fakeIP, err := m.ForwardDNSQuery(domain, gatewayNodeID)
	if err != nil {
		return nil, err
	}
	return fakeIP, nil
}

// RegisterPeer is called when a P2P peer with mesh capability connects.
func (m *MeshManager) RegisterPeer(peerNodeID, peerVIP string) {
	m.topology.AddDirectLink(m.nodeID, m.vip.String(), peerNodeID, peerVIP)
	// Update local node's VIPs in topology to include all VIPs
	allVIPs := m.GetAllVIPs()
	m.topology.SetNodeVIPs(m.nodeID, allVIPs)
	m.recomputeRoutes()
	util.LogInfo("[MESH] peer registered: %s vip=%s", peerNodeID, peerVIP)
}

// UnregisterPeer is called when a P2P peer disconnects.
func (m *MeshManager) UnregisterPeer(peerNodeID string) {
	m.topology.RemoveNode(peerNodeID)
	m.recomputeRoutes()
	util.LogInfo("[MESH] peer unregistered: %s", peerNodeID)
}

// HandleOutboundPacket is the TUN readLoop interceptor.
// Returns true if the packet was handled (destined for mesh or matches a gateway route).
// Sends raw IP packets directly over P2P (no mesh frame header).
func (m *MeshManager) HandleOutboundPacket(dstIP net.IP, data []byte) bool {
	if m.isLocalVIP(dstIP) {
		go func() {
			if m.tun != nil {
				if err := m.tun.WriteMeshPacket(data); err != nil {
					util.LogWarn("[MESH] write local packet to TUN failed: %v", err)
				}
			}
		}()
		return true
	}

	nextHop := m.findNextHop(dstIP)
	if nextHop == nil {
		return false
	}

	// Apply NAT if enabled and source is not a mesh address
	pkt := make([]byte, len(data))
	copy(pkt, data)
	if m.natTable != nil {
		srcIP := extractSrcIP(pkt)
		if srcIP != nil && !isMeshAddress(srcIP) {
			natPkt := m.natTable.TranslateOutbound(pkt)
			if natPkt != nil {
				pkt = natPkt
			}
		}
	}

	go func() {
		if err := m.p2p.SendMeshPacketByVIP(nextHop, pkt); err != nil {
			util.LogWarn("[MESH] send to %s failed: %v", nextHop, err)
		}
	}()
	return true
}

// HandleMeshFrame processes a raw IP packet received from a peer.
// In mesh v2, packets are sent without a mesh frame header.
func (m *MeshManager) HandleMeshFrame(fromNodeID string, frame []byte) {
	if len(frame) < 20 || frame[0]>>4 != 4 {
		util.LogWarn("[MESH] bad packet from %s: %d bytes", fromNodeID, len(frame))
		return
	}

	dstIP := extractDstIP(frame)
	if dstIP == nil {
		util.LogWarn("[MESH] bad packet from %s: cannot extract dst IP", fromNodeID)
		return
	}

	// Check if this packet is for us (dstIP is local VIP or in our subnet)
	if m.isLocalVIP(dstIP) || m.isLocalSubnet(dstIP) {
		pkt := make([]byte, len(frame))
		copy(pkt, frame)

		// Apply reverse NAT if enabled and dstIP is our VIP
		if m.natTable != nil && m.isLocalVIP(dstIP) {
			natPkt := m.natTable.TranslateInbound(pkt)
			if natPkt != nil {
				pkt = natPkt
			}
		}

		// Determine delivery: WriteMeshPacket for VIP, InjectMeshPacket for others
		if m.isLocalVIP(dstIP) {
			go func() {
				if m.tun != nil {
					if err := m.tun.WriteMeshPacket(pkt); err != nil {
						util.LogWarn("[MESH] write to TUN failed: %v", err)
					}
				}
			}()
		} else {
			go func() {
				if m.tun != nil {
					if err := m.tun.InjectMeshPacket(pkt); err != nil {
						util.LogWarn("[MESH] inject to netstack failed: %v", err)
					}
				}
			}()
		}
		return
	}

	// Not for us — check TTL and forward
	if frame[8] <= 1 {
		util.LogDebug("[MESH] TTL expired for packet to %s", dstIP)
		return
	}

	nextHop := m.findNextHop(dstIP)
	if nextHop == nil {
		util.LogDebug("[MESH] forward: no route to %s", dstIP)
		return
	}

	// Decrement TTL and forward
	pkt := make([]byte, len(frame))
	copy(pkt, frame)
	decrementIPTTL(pkt)
	go func() {
		if err := m.p2p.SendMeshPacketByVIP(nextHop, pkt); err != nil {
			util.LogWarn("[MESH] forward to %s failed: %v", nextHop, err)
		}
	}()
}

// isLocalSubnet checks if the IP is in this node's subnet.
func (m *MeshManager) isLocalSubnet(ip net.IP) bool {
	if m.subnet == nil {
		return false
	}
	return m.subnet.Contains(ip)
}

// HandleTopologyGossip processes a topology announcement from a peer.
func (m *MeshManager) HandleTopologyGossip(fromNodeID string, data []byte) {
	var info TopologyInfo
	if err := json.Unmarshal(data, &info); err != nil {
		util.LogDebug("[MESH] bad gossip from %s: %v", fromNodeID, err)
		return
	}
	if info.NodeID == m.nodeID {
		return
	}
	util.LogDebug("[MESH] gossip: received from %s with VIPs=%v", fromNodeID, info.VIPs)
	if m.topology.UpdateFromGossip(info) {
		m.recomputeRoutes()
	}
}

// GetStatus returns mesh status for admin API.
func (m *MeshManager) GetStatus() map[string]interface{} {
	m.prefixRoutesMu.RLock()
	routeCount := len(m.prefixRoutes)
	m.prefixRoutesMu.RUnlock()

	allVIPs := make([]string, 0, len(m.localVIPs))
	for s := range m.localVIPs {
		allVIPs = append(allVIPs, s)
	}

	return map[string]interface{}{
		"enabled":        true,
		"nodeId":         m.nodeID,
		"vip":            m.vip.String(),
		"vips":           allVIPs,
		"subnet":         m.subnetStr,
		"domainSuffixes": m.domainSuffixes,
		"advertise":      m.advertise,
		"routeCount":     routeCount,
	}
}

// GetTopology returns the full topology for admin API.
func (m *MeshManager) GetTopology() map[string]interface{} {
	nodes := m.topology.GetAllNodes()
	nodeList := make([]map[string]interface{}, 0, len(nodes))
	for _, n := range nodes {
		links := make([]LinkInfo, 0, len(n.Links))
		for _, l := range n.Links {
			links = append(links, LinkInfo{PeerNodeID: l.PeerNodeID, Cost: l.Cost})
		}
		entry := map[string]interface{}{
			"nodeId":   n.NodeID,
			"lastSeen": n.LastSeen,
			"links":    links,
		}
		if len(n.VIPs) > 0 {
			vips := make([]string, 0, len(n.VIPs))
			for _, v := range n.VIPs {
				vips = append(vips, v.String())
			}
			entry["vips"] = vips
		}
		if n.Subnet != "" {
			entry["subnet"] = n.Subnet
		}
		if len(n.DomainSuffixes) > 0 {
			entry["domainSuffixes"] = n.DomainSuffixes
		}
		if len(n.Routes) > 0 {
			routes := make([]map[string]interface{}, 0, len(n.Routes))
			for _, r := range n.Routes {
				routes = append(routes, map[string]interface{}{
					"prefix": r.Prefix,
					"cost":   r.Cost,
				})
			}
			entry["routes"] = routes
		}
		nodeList = append(nodeList, entry)
	}
	return map[string]interface{}{"nodes": nodeList}
}

// GetRoutes returns the current routing table for admin API.
func (m *MeshManager) GetRoutes() map[string]interface{} {
	m.prefixRoutesMu.RLock()
	defer m.prefixRoutesMu.RUnlock()

	routes := make([]map[string]interface{}, 0, len(m.prefixRoutes))
	for _, r := range m.prefixRoutes {
		routes = append(routes, map[string]interface{}{
			"prefix":  r.Prefix.String(),
			"nextHop": r.NextHop.String(),
			"cost":    r.Cost,
		})
	}
	return map[string]interface{}{"routes": routes}
}

// GetPeers returns connected mesh peers for admin API.
func (m *MeshManager) GetPeers() []MeshPeerInfo {
	if m.p2p == nil {
		return nil
	}
	ids := m.p2p.ListMeshPeerIDs()
	result := make([]MeshPeerInfo, 0, len(ids))
	for _, id := range ids {
		info := MeshPeerInfo{NodeID: id, Direct: true}
		m.topology.mu.RLock()
		if node, ok := m.topology.nodes[id]; ok && len(node.VIPs) > 0 {
			info.VIP = node.VIPs[0].String()
			info.LastSeen = node.LastSeen
		}
		m.topology.mu.RUnlock()
		result = append(result, info)
	}
	return result
}

// ResolveMeshDomain resolves a mesh domain (e.g., "node.phn") to its VIP.
// Returns nil if the domain is not a mesh domain or the node is unknown.
func (m *MeshManager) ResolveMeshDomain(domain string) net.IP {
	nodeID := ParseNodeDomain(domain)
	if nodeID == "" {
		return nil
	}
	vip := m.topology.GetNodeVIP(nodeID)
	if vip != nil {
		util.LogInfo("[MESH] DNS resolve: %s -> %s", domain, vip)
	}
	return vip
}

func (m *MeshManager) recomputeRoutes() {
	nodes := m.topology.GetAllNodes()
	util.LogDebug("[MESH] recomputeRoutes: topology has %d nodes", len(nodes))
	for _, node := range nodes {
		vips := "nil"
		if len(node.VIPs) > 0 {
			vipStrs := make([]string, len(node.VIPs))
			for i, v := range node.VIPs {
				vipStrs[i] = v.String()
			}
			vips = strings.Join(vipStrs, ",")
		}
		util.LogDebug("[MESH]   node %s vips=%s links=%d", node.NodeID, vips, len(node.Links))
	}

	// 1. Compute node-level routes using Dijkstra
	nodeRoutes := m.topology.ComputeRoutes(m.nodeID)
	util.LogDebug("[MESH] recomputeRoutes: nodeRoutes=%v", nodeRoutes)

	// 2. Build prefix routes
	var prefixRoutes []PrefixRoute

	// Add mesh internal routes (VIP/32 → nextHop VIP)
	for dstNodeID, nextHopNodeID := range nodeRoutes {
		nextHopVIP := m.topology.GetNodeVIP(nextHopNodeID)
		if nextHopVIP == nil {
			continue
		}
		// Add routes for all VIPs of the destination node
		dstVIPs := m.topology.GetNodeAllVIPs(dstNodeID)
		for _, vip := range dstVIPs {
			if vip == nil {
				continue
			}
			prefixRoutes = append(prefixRoutes, PrefixRoute{
				Prefix:  &net.IPNet{IP: vip, Mask: net.CIDRMask(32, 32)},
				NextHop: nextHopVIP,
				Cost:    0,
			})
		}
	}

	// Add gateway-advertised prefix routes
	gatewayRoutes := m.topology.GetAllGatewayRoutes()
	for _, gr := range gatewayRoutes {
		if gr.NodeID == m.nodeID {
			continue // Skip own routes
		}
		_, ipNet, err := net.ParseCIDR(gr.Prefix)
		if err != nil {
			continue
		}
		// Find next hop to the gateway node
		nextHopNodeID, ok := nodeRoutes[gr.NodeID]
		if !ok {
			// Gateway is a direct peer
			nextHopNodeID = gr.NodeID
		}
		nextHopVIP := m.topology.GetNodeVIP(nextHopNodeID)
		if nextHopVIP != nil {
			prefixRoutes = append(prefixRoutes, PrefixRoute{
				Prefix:  ipNet,
				NextHop: nextHopVIP,
				Cost:    gr.Cost,
			})
		}
	}

	// Add subnet-based routes: each node's subnet routes to that node
	for _, node := range nodes {
		if node.NodeID == m.nodeID {
			continue // Skip own subnet
		}
		if node.Subnet == "" {
			continue
		}
		_, subnetNet, err := net.ParseCIDR(node.Subnet)
		if err != nil {
			continue
		}
		nextHopNodeID, ok := nodeRoutes[node.NodeID]
		if !ok {
			nextHopNodeID = node.NodeID
		}
		nextHopVIP := m.topology.GetNodeVIP(nextHopNodeID)
		if nextHopVIP != nil {
			prefixRoutes = append(prefixRoutes, PrefixRoute{
				Prefix:  subnetNet,
				NextHop: nextHopVIP,
				Cost:    0,
			})
		}
	}

	// 3. Sort by prefix length (longest first for longest prefix match)
	sort.Slice(prefixRoutes, func(i, j int) bool {
		lenI, _ := prefixRoutes[i].Prefix.Mask.Size()
		lenJ, _ := prefixRoutes[j].Prefix.Mask.Size()
		return lenI > lenJ
	})

	// 4. Build domain trie from all nodes' domain suffixes
	domainTrie := BuildFromTopology(m.topology)

	m.prefixRoutesMu.Lock()
	m.prefixRoutes = prefixRoutes
	m.domainTrie = domainTrie
	m.prefixRoutesMu.Unlock()
	util.LogInfo("[MESH] routes recomputed: %d prefix routes", len(prefixRoutes))
}

// findNextHop finds the next hop VIP for a destination IP using longest prefix match.
func (m *MeshManager) findNextHop(dstIP net.IP) net.IP {
	m.prefixRoutesMu.RLock()
	defer m.prefixRoutesMu.RUnlock()

	for _, route := range m.prefixRoutes {
		if route.Prefix.Contains(dstIP) {
			return route.NextHop
		}
	}
	return nil
}

// FindGatewayForDomain finds the best gateway for a domain using the domain suffix trie.
// Returns the gateway nodeID, the next hop VIP to reach that gateway, and the matched suffix length.
// Returns ("", nil, 0) if no match.
func (m *MeshManager) FindGatewayForDomain(domain string) (string, net.IP, int) {
	m.prefixRoutesMu.RLock()
	trie := m.domainTrie
	m.prefixRoutesMu.RUnlock()

	if trie == nil {
		return "", nil, 0
	}
	nodeID, suffixLen := trie.Lookup(domain)
	if nodeID == "" {
		return "", nil, 0
	}
	// Find next hop to the gateway
	nextHop := m.findNextHopForNode(nodeID)
	return nodeID, nextHop, suffixLen
}

// findNextHopForNode returns the next hop VIP to reach a given node.
func (m *MeshManager) findNextHopForNode(nodeID string) net.IP {
	nodeRoutes := m.topology.ComputeRoutes(m.nodeID)
	nextHopNodeID, ok := nodeRoutes[nodeID]
	if !ok {
		nextHopNodeID = nodeID
	}
	return m.topology.GetNodeVIP(nextHopNodeID)
}

func (m *MeshManager) gossipLoop() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.closeCh:
			return
		case <-ticker.C:
			// Collect all VIPs
			allVIPs := make([]net.IP, 0, len(m.localVIPs))
			for s := range m.localVIPs {
				if ip := net.ParseIP(s); ip != nil {
					allVIPs = append(allVIPs, ip)
				}
			}
			info := m.topology.GetLocalInfo(m.nodeID, m.subnetStr, m.domainSuffixes, m.advertise, allVIPs)
			data, err := json.Marshal(info)
			if err != nil {
				continue
			}
			if m.p2p != nil {
				m.p2p.BroadcastMeshGossip(data)
			}
		}
	}
}
