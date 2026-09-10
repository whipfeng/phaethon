package mesh

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
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
	mu        sync.RWMutex
	nodeID    string
	vip       net.IP
	localVIPs map[string]bool // all local VIPs (primary + additional) as string keys
	subnet    *net.IPNet
	advertise []string // prefixes this node advertises (e.g., ["0.0.0.0/0", "192.168.1.0/24"])
	topology  *Topology
	tun       TunInterface
	p2p       P2PTransport

	prefixRoutesMu sync.RWMutex
	prefixRoutes   []PrefixRoute // Sorted by prefix length (longest first)

	closeCh chan struct{}
}

// NewMeshManager creates a new mesh manager.
func NewMeshManager(nodeID string, vip net.IP, additionalVIPs []net.IP, subnet *net.IPNet, advertise []string) *MeshManager {
	localVIPs := map[string]bool{
		vip.To4().String(): true,
	}
	for _, v := range additionalVIPs {
		if v4 := v.To4(); v4 != nil {
			localVIPs[v4.String()] = true
		}
	}
	return &MeshManager{
		nodeID:       nodeID,
		vip:          vip.To4(),
		localVIPs:    localVIPs,
		subnet:       subnet,
		advertise:    advertise,
		topology:     NewTopology(),
		prefixRoutes: make([]PrefixRoute, 0),
		closeCh:      make(chan struct{}),
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
// Returns true if the packet was handled (destined for mesh).
// The actual send is async to avoid blocking the readLoop on TCP writes.
func (m *MeshManager) HandleOutboundPacket(dstIP net.IP, data []byte) bool {
	if !m.isMeshDestined(dstIP) {
		return false
	}

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
		util.LogDebug("[MESH] no route to %s, dropping", dstIP)
		return true
	}

	srcVIP := m.vip
	frame := encodeMeshFrame(nextHop, srcVIP, defaultTTL, data)
	go func() {
		if err := m.p2p.SendMeshPacketByVIP(nextHop, frame); err != nil {
			util.LogWarn("[MESH] send to %s failed: %v", nextHop, err)
		}
	}()
	return true
}

// HandleMeshFrame processes a FrameMeshPacket received from a peer.
// Decode is synchronous (fast), but actual I/O is async to avoid blocking the P2P read loop.
func (m *MeshManager) HandleMeshFrame(fromNodeID string, frame []byte) {
	dstVIP, srcVIP, ttl, ipPacket, err := decodeMeshFrame(frame)
	if err != nil {
		util.LogWarn("[MESH] bad frame from %s: %v", fromNodeID, err)
		return
	}

	if m.isLocalVIP(dstVIP) {
		// Local delivery: inject IP packet into OS via TUN device (async).
		pkt := make([]byte, len(ipPacket))
		copy(pkt, ipPacket)
		go func() {
			if m.tun != nil {
				if err := m.tun.WriteMeshPacket(pkt); err != nil {
					util.LogWarn("[MESH] write to TUN failed: %v", err)
				}
			}
		}()
		return
	}

	if ttl <= 1 {
		util.LogDebug("[MESH] TTL expired for %s → %s", srcVIP, dstVIP)
		return
	}

	nextHop := m.findNextHop(dstVIP)
	if nextHop == nil {
		util.LogDebug("[MESH] forward: no route to %s", dstVIP)
		return
	}

	decrementTTL(frame)
	go func() {
		if err := m.p2p.SendMeshPacketByVIP(nextHop, frame); err != nil {
			util.LogWarn("[MESH] forward to %s failed: %v", nextHop, err)
		}
	}()
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
	util.LogInfo("[MESH] gossip: received from %s with VIPs=%v", fromNodeID, info.VIPs)
	changed := m.topology.UpdateFromGossip(info)
	util.LogInfo("[MESH] gossip: topology changed=%v", changed)
	if changed {
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
		"enabled":    true,
		"nodeId":     m.nodeID,
		"vip":        m.vip.String(),
		"vips":       allVIPs,
		"advertise":  m.advertise,
		"routeCount": routeCount,
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

func (m *MeshManager) isMeshDestined(ip net.IP) bool {
	return m.subnet.Contains(ip)
}

func (m *MeshManager) recomputeRoutes() {
	nodes := m.topology.GetAllNodes()
	util.LogInfo("[MESH] recomputeRoutes: topology has %d nodes", len(nodes))
	for _, node := range nodes {
		vips := "nil"
		if len(node.VIPs) > 0 {
			vipStrs := make([]string, len(node.VIPs))
			for i, v := range node.VIPs {
				vipStrs[i] = v.String()
			}
			vips = strings.Join(vipStrs, ",")
		}
		util.LogInfo("[MESH]   node %s vips=%s links=%d", node.NodeID, vips, len(node.Links))
	}

	// 1. Compute node-level routes using Dijkstra
	nodeRoutes := m.topology.ComputeRoutes(m.nodeID)
	util.LogInfo("[MESH] recomputeRoutes: nodeRoutes=%v", nodeRoutes)

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
		util.LogInfo("[MESH] adding routes for node %s: %d VIPs, nextHop=%s, vips=%v", dstNodeID, len(dstVIPs), nextHopVIP, dstVIPs)
		for i, vip := range dstVIPs {
			util.LogInfo("[MESH] processing VIP[%d]: %v (is nil: %v)", i, vip, vip == nil)
			if vip == nil {
				continue
			}
			prefixRoutes = append(prefixRoutes, PrefixRoute{
				Prefix:  &net.IPNet{IP: vip, Mask: net.CIDRMask(32, 32)},
				NextHop: nextHopVIP,
				Cost:    0,
			})
			util.LogInfo("[MESH] route: %s/32 -> %s", vip, nextHopVIP)
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

	// 3. Sort by prefix length (longest first for longest prefix match)
	sort.Slice(prefixRoutes, func(i, j int) bool {
		lenI, _ := prefixRoutes[i].Prefix.Mask.Size()
		lenJ, _ := prefixRoutes[j].Prefix.Mask.Size()
		return lenI > lenJ
	})

	m.prefixRoutesMu.Lock()
	m.prefixRoutes = prefixRoutes
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
			info := m.topology.GetLocalInfo(m.nodeID, m.advertise, allVIPs)
			data, err := json.Marshal(info)
			if err != nil {
				continue
			}
			util.LogInfo("[MESH] gossip: sending with VIPs=%v", info.VIPs)
			if m.p2p != nil {
				m.p2p.BroadcastMeshGossip(data)
			}
		}
	}
}
