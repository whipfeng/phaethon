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
	VIP      net.IP
	Links    map[string]*TopoLink // peerNodeID → link
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
	NodeID string     `json:"nodeId"`
	VIP    string     `json:"vip"`
	Links  []LinkInfo `json:"links"`
}

// LinkInfo is a serializable link entry.
type LinkInfo struct {
	PeerNodeID string `json:"peerNodeId"`
	Cost       int    `json:"cost"`
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

	vip := net.ParseIP(info.VIP)
	if vip != nil {
		node.VIP = vip
	}
	node.LastSeen = time.Now()

	changed := false
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

	return changed
}

// EnsureNode creates a node in the topology if it doesn't exist.
func (t *Topology) EnsureNode(nodeID, vip string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if _, exists := t.nodes[nodeID]; !exists {
		t.nodes[nodeID] = &TopoNode{
			NodeID:  nodeID,
			VIP:     net.ParseIP(vip),
			Links:   make(map[string]*TopoLink),
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
			VIP:    net.ParseIP(thisVIP),
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
			peer.VIP = net.ParseIP(peerVIP)
		}
		t.nodes[peerNodeID] = peer
	} else if peer.VIP == nil && peerVIP != "" {
		peer.VIP = net.ParseIP(peerVIP)
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
func (t *Topology) GetLocalInfo(nodeID string) TopologyInfo {
	t.mu.RLock()
	defer t.mu.RUnlock()

	info := TopologyInfo{NodeID: nodeID}
	node, ok := t.nodes[nodeID]
	if !ok {
		return info
	}
	if node.VIP != nil {
		info.VIP = node.VIP.String()
	}
	for _, link := range node.Links {
		info.Links = append(info.Links, LinkInfo{
			PeerNodeID: link.PeerNodeID,
			Cost:       link.Cost,
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

// VIPToNodeID maps a VIP address to a node ID.
func (t *Topology) VIPToNodeID(vip net.IP) string {
	t.mu.RLock()
	defer t.mu.RUnlock()

	for _, node := range t.nodes {
		if node.VIP != nil && node.VIP.Equal(vip) {
			return node.NodeID
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
			VIP:      n.VIP,
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
