package mesh

import (
	"container/heap"
	"net"
	"sync"
	"time"
)

// Topology tracks the mesh network graph and computes routing tables.
type Topology struct {
	mu    sync.RWMutex
	nodes map[string]*TopoNode
}

// TopoNode represents a node in the mesh topology.
type TopoNode struct {
	NodeID   string
	VIPs     []net.IP           // all VIPs this node owns (first is primary)
	Links    map[string]*TopoLink // peerNodeID → link
	Routes   []RouteInfo        // advertised prefix routes
	LastSeen time.Time
}

// TopoLink represents a direct link between two nodes.
type TopoLink struct {
	PeerNodeID string
	Cost       int
	LastSeen   time.Time
}

// TopologyInfo is the gossip payload exchanged between nodes.
type TopologyInfo struct {
	NodeID string      `json:"nodeId"`
	VIPs   []string    `json:"vips"`
	Links  []LinkInfo  `json:"links"`
	Routes []RouteInfo `json:"routes,omitempty"`
}

// LinkInfo is a serializable link entry.
type LinkInfo struct {
	PeerNodeID string `json:"peerNodeId"`
	PeerVIP    string `json:"peerVip,omitempty"`
	Cost       int    `json:"cost"`
}

// RouteInfo advertises a prefix that this node can reach.
type RouteInfo struct {
	Prefix string `json:"prefix"` // CIDR like "0.0.0.0/0" or "192.168.1.0/24"
	Cost   int    `json:"cost"`   // additional cost (default 0)
}

// NewTopology creates an empty topology graph.
func NewTopology() *Topology {
	return &Topology{
		nodes: make(map[string]*TopoNode),
	}
}

// UpdateFromGossip merges a topology announcement from a peer.
// Returns true if the topology changed (routes may need recomputation).
func (t *Topology) UpdateFromGossip(info TopologyInfo) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	node, exists := t.nodes[info.NodeID]
	if !exists {
		node = &TopoNode{
			NodeID: info.NodeID,
			Links:  make(map[string]*TopoLink),
		}
		t.nodes[info.NodeID] = node
	}

	// Update VIPs
	if len(info.VIPs) > 0 {
		newVIPs := make([]net.IP, 0, len(info.VIPs))
		for _, v := range info.VIPs {
			if ip := net.ParseIP(v); ip != nil {
				newVIPs = append(newVIPs, ip)
			}
		}
		node.VIPs = newVIPs
	}
	node.LastSeen = time.Now()

	changed := false

	// Update advertised routes
	if len(info.Routes) > 0 || len(node.Routes) > 0 {
		if len(info.Routes) != len(node.Routes) {
			changed = true
		}
		node.Routes = info.Routes
	}

	newLinks := make(map[string]bool)
	for _, li := range info.Links {
		newLinks[li.PeerNodeID] = true
		if _, ok := node.Links[li.PeerNodeID]; !ok {
			changed = true
		}
		node.Links[li.PeerNodeID] = &TopoLink{
			PeerNodeID: li.PeerNodeID,
			Cost:       li.Cost,
			LastSeen:   time.Now(),
		}
	}
	for peerID := range node.Links {
		if !newLinks[peerID] {
			changed = true
			delete(node.Links, peerID)
		}
	}

	// Learn neighbor VIPs from the gossip
	for _, li := range info.Links {
		if li.PeerVIP != "" {
			if peerNode, exists := t.nodes[li.PeerNodeID]; !exists {
				t.nodes[li.PeerNodeID] = &TopoNode{
					NodeID:   li.PeerNodeID,
					VIPs:     []net.IP{net.ParseIP(li.PeerVIP)},
					Links:    make(map[string]*TopoLink),
					LastSeen: time.Now(),
				}
				changed = true
			} else if len(peerNode.VIPs) == 0 {
				peerNode.VIPs = []net.IP{net.ParseIP(li.PeerVIP)}
				changed = true
			}
		}
	}

	return changed
}

// EnsureNode creates a node in the topology if it doesn't exist.
func (t *Topology) EnsureNode(nodeID, vip string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if _, exists := t.nodes[nodeID]; !exists {
		t.nodes[nodeID] = &TopoNode{
			NodeID:   nodeID,
			VIPs:     []net.IP{net.ParseIP(vip)},
			Links:    make(map[string]*TopoLink),
			LastSeen: time.Now(),
		}
	}
}

// AddDirectLink records a direct link from thisNode to peerNode.
func (t *Topology) AddDirectLink(thisNodeID, thisVIP string, peerNodeID, peerVIP string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	node, exists := t.nodes[thisNodeID]
	if !exists {
		node = &TopoNode{
			NodeID: thisNodeID,
			VIPs:   []net.IP{net.ParseIP(thisVIP)},
			Links:  make(map[string]*TopoLink),
		}
		t.nodes[thisNodeID] = node
	}

	node.Links[peerNodeID] = &TopoLink{
		PeerNodeID: peerNodeID,
		Cost:       1,
		LastSeen:   time.Now(),
	}
	node.LastSeen = time.Now()

	peer, exists := t.nodes[peerNodeID]
	if !exists {
		peer = &TopoNode{
			NodeID:   peerNodeID,
			Links:    make(map[string]*TopoLink),
			LastSeen: time.Now(),
		}
		if peerVIP != "" {
			peer.VIPs = []net.IP{net.ParseIP(peerVIP)}
		}
		t.nodes[peerNodeID] = peer
	} else if len(peer.VIPs) == 0 && peerVIP != "" {
		peer.VIPs = []net.IP{net.ParseIP(peerVIP)}
	}
	peer.Links[thisNodeID] = &TopoLink{
		PeerNodeID: thisNodeID,
		Cost:       1,
		LastSeen:   time.Now(),
	}
}

// RemoveNode removes a node and all links to it.
func (t *Topology) RemoveNode(nodeID string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	delete(t.nodes, nodeID)
	for _, n := range t.nodes {
		delete(n.Links, nodeID)
	}
}

// GetLocalInfo returns this node's topology info for gossip.
func (t *Topology) GetLocalInfo(nodeID string, advertise []string, allVIPs []net.IP) TopologyInfo {
	t.mu.RLock()
	defer t.mu.RUnlock()

	info := TopologyInfo{NodeID: nodeID}
	node, ok := t.nodes[nodeID]
	if !ok {
		return info
	}
	// Include all VIPs
	for _, v := range allVIPs {
		info.VIPs = append(info.VIPs, v.String())
	}
	for _, link := range node.Links {
		peerVIP := ""
		if peerNode, ok := t.nodes[link.PeerNodeID]; ok && len(peerNode.VIPs) > 0 {
			peerVIP = peerNode.VIPs[0].String()
		}
		info.Links = append(info.Links, LinkInfo{
			PeerNodeID: link.PeerNodeID,
			PeerVIP:    peerVIP,
			Cost:       link.Cost,
		})
	}
	// Advertise configured prefixes
	for _, prefix := range advertise {
		info.Routes = append(info.Routes, RouteInfo{
			Prefix: prefix,
			Cost:   0,
		})
	}
	return info
}

// ComputeRoutes runs Dijkstra from myNodeID and returns
// a map of dstNodeID → nextHopNodeID.
func (t *Topology) ComputeRoutes(myNodeID string) map[string]string {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if _, ok := t.nodes[myNodeID]; !ok {
		return nil
	}

	dist := make(map[string]int)
	prev := make(map[string]string)
	visited := make(map[string]bool)

	for id := range t.nodes {
		dist[id] = int(^uint(0) >> 1)
	}
	dist[myNodeID] = 0

	pq := &priorityQueue{}
	heap.Init(pq)
	heap.Push(pq, &pqItem{nodeID: myNodeID, dist: 0})

	for pq.Len() > 0 {
		cur := heap.Pop(pq).(*pqItem)
		if visited[cur.nodeID] {
			continue
		}
		visited[cur.nodeID] = true

		node := t.nodes[cur.nodeID]
		if node == nil {
			continue
		}
		for peerID, link := range node.Links {
			if visited[peerID] {
				continue
			}
			alt := dist[cur.nodeID] + link.Cost
			if alt < dist[peerID] {
				dist[peerID] = alt
				prev[peerID] = cur.nodeID
				heap.Push(pq, &pqItem{nodeID: peerID, dist: alt})
			}
		}
	}

	routes := make(map[string]string)
	for dstID := range t.nodes {
		if dstID == myNodeID {
			continue
		}
		if _, reachable := dist[dstID]; !reachable {
			continue
		}
		if dist[dstID] == int(^uint(0)>>1) {
			continue
		}
		next := dstID
		for prev[next] != myNodeID && prev[next] != "" {
			next = prev[next]
		}
		if prev[next] == myNodeID || next == dstID {
			routes[dstID] = next
		}
	}
	return routes
}

// GetNodeVIP returns the primary VIP of a node by its nodeID.
func (t *Topology) GetNodeVIP(nodeID string) net.IP {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if node, ok := t.nodes[nodeID]; ok && len(node.VIPs) > 0 {
		return node.VIPs[0]
	}
	return nil
}

// GetNodeAllVIPs returns all VIPs of a node by its nodeID.
func (t *Topology) GetNodeAllVIPs(nodeID string) []net.IP {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if node, ok := t.nodes[nodeID]; ok {
		return node.VIPs
	}
	return nil
}

// GatewayRoute represents a prefix advertised by a gateway node.
type GatewayRoute struct {
	NodeID string // Gateway node ID
	Prefix string // Advertised prefix (e.g., "0.0.0.0/0")
	Cost   int    // Advertised cost
}

// GetAllGatewayRoutes collects all prefix advertisements from all nodes.
func (t *Topology) GetAllGatewayRoutes() []GatewayRoute {
	t.mu.RLock()
	defer t.mu.RUnlock()

	var routes []GatewayRoute
	for _, node := range t.nodes {
		// Check if this node has advertised routes
		// We need to store this information when processing gossip
		// For now, we'll check if the node has any routes stored
		if node.Routes != nil {
			for _, r := range node.Routes {
				routes = append(routes, GatewayRoute{
					NodeID: node.NodeID,
					Prefix: r.Prefix,
					Cost:   r.Cost,
				})
			}
		}
	}
	return routes
}

// VIPToNodeID maps a VIP address to a node ID.
func (t *Topology) VIPToNodeID(vip net.IP) string {
	t.mu.RLock()
	defer t.mu.RUnlock()

	for _, node := range t.nodes {
		for _, v := range node.VIPs {
			if v != nil && v.Equal(vip) {
				return node.NodeID
			}
		}
	}
	return ""
}

// GetAllNodes returns a snapshot of all nodes for admin API.
func (t *Topology) GetAllNodes() []TopoNode {
	t.mu.RLock()
	defer t.mu.RUnlock()

	result := make([]TopoNode, 0, len(t.nodes))
	for _, n := range t.nodes {
		links := make([]LinkInfo, 0, len(n.Links))
		for _, l := range n.Links {
			links = append(links, LinkInfo{PeerNodeID: l.PeerNodeID, Cost: l.Cost})
		}
		result = append(result, TopoNode{
			NodeID:   n.NodeID,
			VIPs:     n.VIPs,
			LastSeen: n.LastSeen,
			Links:    make(map[string]*TopoLink),
		})
		for id, l := range n.Links {
			result[len(result)-1].Links[id] = &TopoLink{
				PeerNodeID: l.PeerNodeID,
				Cost:       l.Cost,
				LastSeen:   l.LastSeen,
			}
		}
	}
	return result
}

// priority queue for Dijkstra
type pqItem struct {
	nodeID string
	dist   int
	index  int
}

type priorityQueue []*pqItem

func (pq priorityQueue) Len() int            { return len(pq) }
func (pq priorityQueue) Less(i, j int) bool  { return pq[i].dist < pq[j].dist }
func (pq priorityQueue) Swap(i, j int)       { pq[i], pq[j] = pq[j], pq[i]; pq[i].index = i; pq[j].index = j }
func (pq *priorityQueue) Push(x interface{}) { item := x.(*pqItem); item.index = len(*pq); *pq = append(*pq, item) }
func (pq *priorityQueue) Pop() interface{} {
	old := *pq
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	item.index = -1
	*pq = old[:n-1]
	return item
}
