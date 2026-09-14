package mesh

import (
	"net"
	"sync"
	"time"
)

// PeerRouteEntry represents a route learned from a peer, with hop count.
type PeerRouteEntry struct {
	Prefix    *net.IPNet
	PrefixStr string
	Hop       int // hop count (already incremented on receive)
}

// PeerDomainSuffixEntry represents a domain suffix learned from a peer, with hop count.
type PeerDomainSuffixEntry struct {
	Suffix string
	Hop    int // hop count (already incremented on receive)
}

// PeerClaimedSubnetEntry represents a subnet claim learned via gossip, with hop count.
type PeerClaimedSubnetEntry struct {
	Subnet    *net.IPNet
	SubnetStr string
	NodeID    string
	Hop       int // hop count (already incremented on receive)
}

// PeerInfo holds the information received from a peer via gossip.
// Created by RegisterPeer, destroyed by UnregisterPeer.
// Data follows the connection lifecycle.
type PeerInfo struct {
	Sender         PeerSender
	Subnet         *net.IPNet
	SubnetStr      string
	DomainSuffixes []PeerDomainSuffixEntry
	Routes         []PeerRouteEntry
	ClaimedSubnets []PeerClaimedSubnetEntry
	LastSeen       time.Time
}

// NodeID returns the peer's node ID via its Sender.
func (p *PeerInfo) NodeID() string {
	if p.Sender == nil {
		return ""
	}
	return p.Sender.GetNodeID()
}

// GossipRoute is a serializable route entry with hop count.
type GossipRoute struct {
	Prefix string `json:"prefix"`
	Hop    int    `json:"hop"`
}

// GossipDomainSuffix is a serializable domain suffix entry with hop count.
type GossipDomainSuffix struct {
	Suffix string `json:"suffix"`
	Hop    int    `json:"hop"`
}

// GossipClaimedSubnet is a serializable subnet claim with nodeId and hop count.
type GossipClaimedSubnet struct {
	Subnet string `json:"subnet"`
	NodeID string `json:"nodeId"`
	Hop    int    `json:"hop"`
}

// GossipInfo is the gossip payload exchanged between nodes.
type GossipInfo struct {
	NodeID         string                `json:"nodeId"`
	Subnet         string                `json:"subnet"`
	DomainSuffixes []GossipDomainSuffix  `json:"domainSuffixes,omitempty"`
	Routes         []GossipRoute         `json:"routes,omitempty"`
	ClaimedSubnets []GossipClaimedSubnet `json:"claimedSubnets,omitempty"`
}

// Topology tracks mesh peers and their advertised capabilities.
type Topology struct {
	mu    sync.RWMutex
	peers []*PeerInfo
}

// NewTopology creates an empty topology.
func NewTopology() *Topology {
	return &Topology{}
}

// RegisterPeer creates a new PeerInfo entry for a connected peer.
func (t *Topology) RegisterPeer(sender PeerSender) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.peers = append(t.peers, &PeerInfo{
		Sender:   sender,
		LastSeen: time.Now(),
	})
}

// UnregisterPeer removes a peer entry by Sender pointer.
func (t *Topology) UnregisterPeer(sender PeerSender) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, p := range t.peers {
		if p.Sender == sender {
			t.peers = append(t.peers[:i], t.peers[i+1:]...)
			return
		}
	}
}

// UpdateGossip updates a peer's gossip data in-place.
// Finds the peer by Sender pointer. Returns false if sender not found (data discarded).
func (t *Topology) UpdateGossip(sender PeerSender, info GossipInfo) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	var peer *PeerInfo
	for _, p := range t.peers {
		if p.Sender == sender {
			peer = p
			break
		}
	}
	if peer == nil {
		return false
	}

	// Parse subnet
	var subnet *net.IPNet
	if info.Subnet != "" {
		_, ipNet, err := net.ParseCIDR(info.Subnet)
		if err != nil {
			return false
		}
		subnet = ipNet
	}

	// Parse routes (increment hop count for each entry)
	var routes []PeerRouteEntry
	for _, r := range info.Routes {
		_, ipNet, err := net.ParseCIDR(r.Prefix)
		if err != nil {
			continue
		}
		routes = append(routes, PeerRouteEntry{
			Prefix:    ipNet,
			PrefixStr: r.Prefix,
			Hop:       r.Hop + 1,
		})
	}

	// Parse domain suffixes (increment hop count for each entry)
	var domainSuffixes []PeerDomainSuffixEntry
	for _, ds := range info.DomainSuffixes {
		domainSuffixes = append(domainSuffixes, PeerDomainSuffixEntry{
			Suffix: ds.Suffix,
			Hop:    ds.Hop + 1,
		})
	}

	// Parse claimed subnets (increment hop count for each entry)
	var claimedSubnets []PeerClaimedSubnetEntry
	for _, cs := range info.ClaimedSubnets {
		_, ipNet, err := net.ParseCIDR(cs.Subnet)
		if err != nil {
			continue
		}
		claimedSubnets = append(claimedSubnets, PeerClaimedSubnetEntry{
			Subnet:    ipNet,
			SubnetStr: cs.Subnet,
			NodeID:    cs.NodeID,
			Hop:       cs.Hop + 1,
		})
	}

	// Check if anything changed
	changed := false
	if peer.SubnetStr != info.Subnet {
		changed = true
	}
	if !domainSuffixesEqual(peer.DomainSuffixes, domainSuffixes) {
		changed = true
	}
	if !routesEqual(peer.Routes, routes) {
		changed = true
	}
	if !claimedSubnetsEqual(peer.ClaimedSubnets, claimedSubnets) {
		changed = true
	}

	// Update in-place
	peer.Subnet = subnet
	peer.SubnetStr = info.Subnet
	peer.DomainSuffixes = domainSuffixes
	peer.Routes = routes
	peer.ClaimedSubnets = claimedSubnets
	peer.LastSeen = time.Now()

	return changed
}

// GetAllPeers returns a snapshot of all peers.
func (t *Topology) GetAllPeers() []*PeerInfo {
	t.mu.RLock()
	defer t.mu.RUnlock()

	result := make([]*PeerInfo, len(t.peers))
	copy(result, t.peers)
	return result
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

// routesEqual compares two route slices for equality.
func routesEqual(a, b []PeerRouteEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].PrefixStr != b[i].PrefixStr || a[i].Hop != b[i].Hop {
			return false
		}
	}
	return true
}

// domainSuffixesEqual compares two domain suffix entry slices for equality.
func domainSuffixesEqual(a, b []PeerDomainSuffixEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Suffix != b[i].Suffix || a[i].Hop != b[i].Hop {
			return false
		}
	}
	return true
}

// claimedSubnetsEqual compares two claimed subnet entry slices for equality.
func claimedSubnetsEqual(a, b []PeerClaimedSubnetEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].SubnetStr != b[i].SubnetStr || a[i].NodeID != b[i].NodeID || a[i].Hop != b[i].Hop {
			return false
		}
	}
	return true
}
