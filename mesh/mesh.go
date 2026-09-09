package mesh

import (
	"encoding/json"
	"net"
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

// MeshManager coordinates mesh overlay networking.
type MeshManager struct {
	mu       sync.RWMutex
	nodeID   string
	vip      net.IP
	subnet   *net.IPNet
	topology *Topology
	tun      TunInterface
	p2p      P2PTransport

	routesMu sync.RWMutex
	routes   map[string]string // dstNodeID → nextHopNodeID

	closeCh chan struct{}
}

// NewMeshManager creates a new mesh manager.
func NewMeshManager(nodeID string, vip net.IP, subnet *net.IPNet) *MeshManager {
	return &MeshManager{
		nodeID:   nodeID,
		vip:      vip.To4(),
		subnet:   subnet,
		topology: NewTopology(),
		routes:   make(map[string]string),
		closeCh:  make(chan struct{}),
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

	dstNodeID := m.topology.VIPToNodeID(dstIP)
	if dstNodeID == "" {
		util.LogWarn("[MESH] no route to VIP %s", dstIP)
		return true
	}

	nextHop := m.getNextHop(dstNodeID)
	if nextHop == "" {
		util.LogWarn("[MESH] no next hop for node %s (dst %s)", dstNodeID, dstIP)
		return true
	}

	util.LogDebug("[MESH] forwarding packet to %s via %s", dstIP, nextHop)
	srcVIP := m.vip
	frame := encodeMeshFrame(dstIP, srcVIP, defaultTTL, data)
	if err := m.p2p.SendMeshPacket(nextHop, frame); err != nil {
		util.LogWarn("[MESH] send to %s via %s failed: %v", dstNodeID, nextHop, err)
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

	dstNodeID := m.topology.VIPToNodeID(dstVIP)
	if dstNodeID == "" {
		util.LogDebug("[MESH] forward: no route to %s", dstVIP)
		return
	}

	nextHop := m.getNextHop(dstNodeID)
	if nextHop == "" {
		util.LogDebug("[MESH] forward: no next hop for %s", dstNodeID)
		return
	}

	util.LogDebug("[MESH] forwarding frame %s → %s via %s", dstVIP, srcVIP, nextHop)
	decrementTTL(frame)
	if err := m.p2p.SendMeshPacket(nextHop, frame); err != nil {
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
	m.routesMu.RLock()
	routeCount := len(m.routes)
	m.routesMu.RUnlock()

	return map[string]interface{}{
		"enabled":    true,
		"nodeId":     m.nodeID,
		"vip":        m.vip.String(),
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
	m.routesMu.RLock()
	defer m.routesMu.RUnlock()

	routes := make(map[string]string, len(m.routes))
	for k, v := range m.routes {
		routes[k] = v
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

func (m *MeshManager) getNextHop(dstNodeID string) string {
	m.routesMu.RLock()
	defer m.routesMu.RUnlock()
	return m.routes[dstNodeID]
}

func (m *MeshManager) recomputeRoutes() {
	routes := m.topology.ComputeRoutes(m.nodeID)
	m.routesMu.Lock()
	m.routes = routes
	m.routesMu.Unlock()
	util.LogDebug("[MESH] routes recomputed: %d destinations", len(routes))
}

func (m *MeshManager) gossipLoop() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.closeCh:
			return
		case <-ticker.C:
			info := m.topology.GetLocalInfo(m.nodeID)
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
