package mesh

import (
	"encoding/json"
	"net"
	"sort"
	"sync"
	"time"

	"phaethon/util"
)

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
func NewMeshManager(nodeID string, vip net.IP, subnet *net.IPNet, advertise []string) *MeshManager {
	return &MeshManager{
		nodeID:       nodeID,
		vip:          vip.To4(),
		subnet:       subnet,
		advertise:    advertise,
		topology:     NewTopology(),
		prefixRoutes: make([]PrefixRoute, 0),
		closeCh:      make(chan struct{}),
	}
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
func (m *MeshManager) HandleOutboundPacket(dstIP net.IP, data []byte) bool {
	if !m.isMeshDestined(dstIP) {
		return false
	}
	util.LogDebug("[MESH] outbound packet to %s", dstIP)
	if dstIP.Equal(m.vip) {
		util.LogDebug("[MESH] packet for local VIP %s, injecting", dstIP)
		if m.tun != nil {
			if err := m.tun.InjectMeshPacket(data); err != nil {
				util.LogWarn("[MESH] inject local packet failed: %v", err)
			}
		}
		return true
	}

	// 2. Longest prefix match to find next hop
	nextHop := m.findNextHop(dstIP)
	if nextHop == nil {
		util.LogDebug("[MESH] no route to %s", dstIP)
		return false
	}

	util.LogDebug("[MESH] forwarding packet to %s via %s", dstIP, nextHop)
	srcVIP := m.vip
	frame := encodeMeshFrame(nextHop, srcVIP, defaultTTL, data)
	if err := m.p2p.SendMeshPacketByVIP(nextHop, frame); err != nil {
		util.LogWarn("[MESH] send to %s failed: %v", nextHop, err)
	}
	return true
}

// HandleMeshFrame processes a FrameMeshPacket received from a peer.
func (m *MeshManager) HandleMeshFrame(fromNodeID string, frame []byte) {
	util.LogDebug("[MESH] received frame from %s (%d bytes)", fromNodeID, len(frame))
	dstVIP, srcVIP, ttl, ipPacket, err := decodeMeshFrame(frame)
	if err != nil {
		util.LogWarn("[MESH] bad frame from %s: %v", fromNodeID, err)
		return
	}

	util.LogDebug("[MESH] frame: %s → %s ttl=%d len=%d (local=%s)", srcVIP, dstVIP, ttl, len(ipPacket), m.vip)

	if dstVIP.Equal(m.vip) {
		if m.tun != nil {
			if err := m.tun.WriteMeshPacket(ipPacket); err != nil {
				util.LogWarn("[MESH] write to TUN failed: %v", err)
			} else {
				util.LogDebug("[MESH] delivered %s → %s (%d bytes) to OS via TUN", srcVIP, dstVIP, len(ipPacket))
			}
		} else {
			util.LogWarn("[MESH] TUN engine not set, cannot deliver packet")
		}
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

	util.LogDebug("[MESH] forwarding frame %s → %s via %s", dstVIP, srcVIP, nextHop)
	decrementTTL(frame)
	if err := m.p2p.SendMeshPacketByVIP(nextHop, frame); err != nil {
		util.LogWarn("[MESH] forward to %s failed: %v", nextHop, err)
	}
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
	if m.topology.UpdateFromGossip(info) {
		m.recomputeRoutes()
	}
}

// GetStatus returns mesh status for admin API.
func (m *MeshManager) GetStatus() map[string]interface{} {
	m.prefixRoutesMu.RLock()
	routeCount := len(m.prefixRoutes)
	m.prefixRoutesMu.RUnlock()

	return map[string]interface{}{
		"enabled":    true,
		"nodeId":     m.nodeID,
		"vip":        m.vip.String(),
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
		if n.VIP != nil {
			entry["vip"] = n.VIP.String()
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
		if node, ok := m.topology.nodes[id]; ok {
			if node.VIP != nil {
				info.VIP = node.VIP.String()
			}
			info.LastSeen = node.LastSeen
		}
		m.topology.mu.RUnlock()
		result = append(result, info)
	}
	return result
}

func (m *MeshManager) isMeshDestined(ip net.IP) bool {
	return m.subnet.Contains(ip)
}

func (m *MeshManager) recomputeRoutes() {
	// 1. Compute node-level routes using Dijkstra
	nodeRoutes := m.topology.ComputeRoutes(m.nodeID)

	// 2. Build prefix routes
	var prefixRoutes []PrefixRoute

	// Add mesh internal routes (VIP/32 → nextHop VIP)
	for dstNodeID, nextHopNodeID := range nodeRoutes {
		dstVIP := m.topology.GetNodeVIP(dstNodeID)
		nextHopVIP := m.topology.GetNodeVIP(nextHopNodeID)
		if dstVIP != nil && nextHopVIP != nil {
			prefixRoutes = append(prefixRoutes, PrefixRoute{
				Prefix:  &net.IPNet{IP: dstVIP, Mask: net.CIDRMask(32, 32)},
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

	// 3. Sort by prefix length (longest first for longest prefix match)
	sort.Slice(prefixRoutes, func(i, j int) bool {
		lenI, _ := prefixRoutes[i].Prefix.Mask.Size()
		lenJ, _ := prefixRoutes[j].Prefix.Mask.Size()
		return lenI > lenJ
	})

	m.prefixRoutesMu.Lock()
	m.prefixRoutes = prefixRoutes
	m.prefixRoutesMu.Unlock()
	util.LogDebug("[MESH] routes recomputed: %d prefix routes", len(prefixRoutes))
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
			info := m.topology.GetLocalInfo(m.nodeID, m.advertise)
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
