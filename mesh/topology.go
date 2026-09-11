package mesh

import (
	"net"
	"strings"
	"sync"
	"time"
)

// PeerInfo holds the information received from a peer via gossip.
type PeerInfo struct {
	NodeID         string
	Sender         PeerSender // direct reference to peer connection
	Subnet         *net.IPNet // peer's mesh subnet (also a route)
	SubnetStr      string     // subnet CIDR string
	DomainSuffixes []string   // domain suffixes this peer can resolve
	Routes         []*net.IPNet // additional routes this peer can reach
	LastSeen       time.Time
}

// GossipInfo is the gossip payload exchanged between nodes.
type GossipInfo struct {
	NodeID         string   `json:"nodeId"`
	Subnet         string   `json:"subnet"`
	DomainSuffixes []string `json:"domainSuffixes,omitempty"`
	Routes         []string `json:"routes,omitempty"`
}

// Topology tracks mesh peers and their advertised capabilities.
type Topology struct {
	mu    sync.RWMutex
	peers map[string]*PeerInfo // nodeID → peer info
}

// NewTopology creates an empty topology.
func NewTopology() *Topology {
	return &Topology{
		peers: make(map[string]*PeerInfo),
	}
}

// UpdateFromGossip processes a gossip message from a peer.
// Returns true if the topology changed (routes may need recomputation).
func (t *Topology) UpdateFromGossip(info GossipInfo) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	existing, exists := t.peers[info.NodeID]

	// Parse subnet
	var subnet *net.IPNet
	if info.Subnet != "" {
		_, ipNet, err := net.ParseCIDR(info.Subnet)
		if err != nil {
			return false
		}
		subnet = ipNet
	}

	// Parse routes
	var routes []*net.IPNet
	for _, r := range info.Routes {
		_, ipNet, err := net.ParseCIDR(r)
		if err != nil {
			continue
		}
		routes = append(routes, ipNet)
	}

	// Check if anything changed
	changed := !exists
	if exists {
		if existing.SubnetStr != info.Subnet {
			changed = true
		}
		if len(existing.DomainSuffixes) != len(info.DomainSuffixes) {
			changed = true
		} else {
			for i := range existing.DomainSuffixes {
				if existing.DomainSuffixes[i] != info.DomainSuffixes[i] {
					changed = true
					break
				}
			}
		}
		if len(existing.Routes) != len(routes) {
			changed = true
		}
	}

	// Update peer info, preserving Sender if it was already set
	var sender PeerSender
	if exists && existing != nil {
		sender = existing.Sender
	}
	t.peers[info.NodeID] = &PeerInfo{
		NodeID:         info.NodeID,
		Sender:         sender,
		Subnet:         subnet,
		SubnetStr:      info.Subnet,
		DomainSuffixes: info.DomainSuffixes,
		Routes:         routes,
		LastSeen:       time.Now(),
	}

	return changed
}

// RemovePeer removes a peer from the topology.
func (t *Topology) RemovePeer(nodeID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.peers, nodeID)
}

// RegisterSender stores a PeerSender for a peer, creating the entry if needed.
func (t *Topology) RegisterSender(sender PeerSender) {
	t.mu.Lock()
	defer t.mu.Unlock()
	nodeID := sender.GetNodeID()
	if peer, ok := t.peers[nodeID]; ok {
		peer.Sender = sender
	} else {
		t.peers[nodeID] = &PeerInfo{
			NodeID:   nodeID,
			Sender:   sender,
			LastSeen: time.Now(),
		}
	}
}

// GetAllPeers returns a snapshot of all peers.
func (t *Topology) GetAllPeers() []*PeerInfo {
	t.mu.RLock()
	defer t.mu.RUnlock()

	result := make([]*PeerInfo, 0, len(t.peers))
	for _, p := range t.peers {
		result = append(result, p)
	}
	return result
}

// GetPeer returns a specific peer by nodeID.
func (t *Topology) GetPeer(nodeID string) *PeerInfo {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.peers[nodeID]
}

// FindGatewayByDomainSuffix finds the best gateway for a domain using longest suffix match.
// Returns the peer's nodeID and the matched suffix length, or ("", 0) if no match.
// Domain suffixes should be bare domains like "github.com" (no leading dot).
func (t *Topology) FindGatewayByDomainSuffix(domain string) (string, int) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	domain = strings.TrimPrefix(strings.ToLower(domain), ".")
	bestNodeID := ""
	bestLen := 0

	for _, peer := range t.peers {
		for _, suffix := range peer.DomainSuffixes {
			suffix = strings.TrimPrefix(strings.ToLower(suffix), ".")
			if domain == suffix || strings.HasSuffix(domain, "."+suffix) {
				if len(suffix) > bestLen {
					bestLen = len(suffix)
					bestNodeID = peer.NodeID
				}
			}
		}
	}
	return bestNodeID, bestLen
}

// DeriveVIPFromSubnet computes the .1 VIP address from a subnet.
func DeriveVIPFromSubnet(subnet *net.IPNet) net.IP {
	if subnet == nil {
		return nil
	}
	baseIP := subnet.IP.To4()
	if baseIP == nil {
		return nil
	}
	vip := make(net.IP, 4)
	copy(vip, baseIP)
	vip[3] |= 1
	return vip
}

// DeriveGIPFromSubnet computes the .3 GIP address from a subnet.
func DeriveGIPFromSubnet(subnet *net.IPNet) net.IP {
	if subnet == nil {
		return nil
	}
	baseIP := subnet.IP.To4()
	if baseIP == nil {
		return nil
	}
	gip := make(net.IP, 4)
	copy(gip, baseIP)
	gip[3] |= 3
	return gip
}
