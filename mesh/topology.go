package mesh

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"phaethon/util"
)

// PeerRouteEntry represents a route learned from a peer, referencing the owner node.
type PeerRouteEntry struct {
	Prefix    *net.IPNet
	PrefixStr string
	NodeID    string // route owner
}

// PeerDomainSuffixEntry represents a domain suffix learned from a peer, referencing the owner node.
type PeerDomainSuffixEntry struct {
	Suffix string
	NodeID string // domain suffix owner
}

// PeerClaimedSubnetEntry represents a subnet claim learned via gossip, with hop count and neighbors.
type PeerClaimedSubnetEntry struct {
	Subnet    *net.IPNet
	SubnetStr string
	NodeID    string
	Hop       int              // hop count (already incremented on receive)
	Neighbors []GossipNeighbor // direct neighbors of this node with link quality
}

// summarizeNeighbors renders neighbor node IDs plus their link seq pairs, so a
// log line shows whether a claim carried the link we are looking for.
func summarizeNeighbors(nbrs []GossipNeighbor) string {
	parts := make([]string, 0, len(nbrs))
	for _, n := range nbrs {
		seqs := make([]string, 0, len(n.Links))
		for _, l := range n.Links {
			seqs = append(seqs, fmt.Sprintf("%d/%d", l.LocalSeq, l.RemoteSeq))
		}
		parts = append(parts, fmt.Sprintf("%s(%s)", n.NodeID, strings.Join(seqs, ",")))
	}
	return strings.Join(parts, " ")
}

// summarizePeerClaims renders inbound (post hop-increment) claims for logging.
func summarizePeerClaims(claims []PeerClaimedSubnetEntry) string {
	if len(claims) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(claims))
	for _, c := range claims {
		parts = append(parts, fmt.Sprintf("%s@%s(h%d){%s}",
			c.NodeID, c.SubnetStr, c.Hop, summarizeNeighbors(c.Neighbors)))
	}
	return strings.Join(parts, " ")
}

// summarizeGossipClaims renders outbound claims for logging.
func summarizeGossipClaims(claims []GossipClaimedSubnet) string {
	if len(claims) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(claims))
	for _, c := range claims {
		parts = append(parts, fmt.Sprintf("%s@%s(h%d){%s}",
			c.NodeID, c.Subnet, c.Hop, summarizeNeighbors(c.Neighbors)))
	}
	return strings.Join(parts, " ")
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

// GossipRoute is a serializable route entry referencing the owner node.
type GossipRoute struct {
	Prefix string `json:"prefix"`
	NodeID string `json:"nodeId"` // route owner
}

// GossipDomainSuffix is a serializable domain suffix entry referencing the owner node.
type GossipDomainSuffix struct {
	Suffix string `json:"suffix"`
	NodeID string `json:"nodeId"` // domain suffix owner
}

// GossipLink represents a single link with quality metrics.
type GossipLink struct {
	LocalSeq     uint16  `json:"localSeq"`               // 本端声明的 seq
	RemoteSeq    uint16  `json:"remoteSeq"`              // 对端声明的 seq
	FriendlyName string  `json:"friendlyName,omitempty"` // 友好名称（代理名，仅展示）
	SRTT         float64 `json:"srtt,omitempty"`         // 平滑 RTT (ms)
	LossRate     float64 `json:"lossRate,omitempty"`     // 丢包率 (0.0-1.0)
	Jitter       float64 `json:"jitter,omitempty"`       // 抖动 (ms)
	DropRate     float64 `json:"dropRate,omitempty"`     // 本机出向队列丢包率 (0.0-1.0, Phase B)
	SojournMs    float64 `json:"sojournMs,omitempty"`    // 排队时延 EMA (ms, Phase B)
}

// GossipNeighbor represents a direct neighbor with multiple links.
type GossipNeighbor struct {
	NodeID string       `json:"nodeId"`          // neighbor node ID
	Links  []GossipLink `json:"links,omitempty"` // links to this neighbor
}

// GossipClaimedSubnet is a serializable subnet claim with nodeId, hop count, and neighbors.
type GossipClaimedSubnet struct {
	Subnet    string           `json:"subnet"`
	NodeID    string           `json:"nodeId"`
	Hop       int              `json:"hop"`
	Neighbors []GossipNeighbor `json:"neighbors,omitempty"` // direct neighbors with link quality
}

// GossipTopologyEdge represents a topology edge in gossip messages.
type GossipTopologyEdge struct {
	NodeID   string `json:"nodeId"`   // edge source
	Neighbor string `json:"neighbor"` // edge target
}

// P2PProtocolVersion is the current P2P protocol version, carried in every
// GossipInfo frame (gossip has no separate hello in v8, so the version and
// link Seq travel with the periodic broadcast).
// Bump when making incompatible changes to the P2P frame protocol.
// Version 9: GossipLink changed from linkId to localSeq+remoteSeq fields,
// link identification now based on seq pair matching instead of combined linkId.
// Version 10: v8 simplified control plane formal release - hello/ack frames removed
// (peer registration, version check, and Seq negotiation moved into the first gossip
// frame per design §5.3), ping/pong active link probing, mandatory version negotiation
// (mismatch = disconnect + Warn). Number skips 9 because v9 is already deployed with
// a different GossipLink wire format; reusing 9 would make old nodes accept new peers.
const P2PProtocolVersion = 10

// P2PProtocolVersionMin is the oldest protocol this build accepts. Peers in
// [min, current] negotiate down to the lower version (D25); peers below the
// minimum only fail the current attempt — they keep being probed and
// reconnected, so an upgrade on either side recovers automatically.
const P2PProtocolVersionMin = 9

// Capability names advertised in hello/gossip (D25). Optional features are
// enabled per peer from the intersection of both sides' capability sets.
const (
	// CapabilityStripe means the peer supports dynamic extra data lanes
	// (StripeAttach / FrameLaneAck).
	CapabilityStripe = "stripe"
)

// GossipInfo is the gossip payload exchanged between nodes.
// hello and gossip use the same structure; hello is a superset with Cmd and ProtocolVersion.
type GossipInfo struct {
	// Protocol fields (only used by hello)
	Cmd             string `json:"cmd,omitempty"`             // "hello" or "gossip"
	ProtocolVersion int    `json:"protocolVersion,omitempty"` // only in hello
	// MinProtocolVersion is the oldest version the sender accepts; capability
	// names let new features be enabled per peer (D25).
	MinProtocolVersion int      `json:"minProtocolVersion,omitempty"`
	Capabilities       []string `json:"capabilities,omitempty"`
	Seq                uint16   `json:"seq,omitempty"`          // 本端生成的序列号（hello 协商用）
	FriendlyName       string   `json:"friendlyName,omitempty"` // 友好名称（代理名，hello 协商用）

	// Topology information (both hello and gossip carry this)
	DomainSuffixes []GossipDomainSuffix  `json:"domainSuffixes,omitempty"`
	Routes         []GossipRoute         `json:"routes,omitempty"`
	ClaimedSubnets []GossipClaimedSubnet `json:"claimedSubnets,omitempty"`
	// TopologyEdges removed - topology expressed via ClaimedSubnets.Neighbors

	// UDP leg negotiation (per-link, docs/plans/p2p_udp_transport_design.md §3):
	// UDP=true declares this link supports the optional UDP data leg;
	// UDPPort echoes the allocated leg port (passive side, dst 0.0.0.0:P).
	UDP     bool `json:"udp,omitempty"`
	UDPPort int  `json:"udpPort,omitempty"`

	// LaneAck in-flight gating (per-link, docs/plans/mesh-control-isolation-
	// inflight-cap.md §3.2): gossip is the periodic fallback channel — every
	// declaration/report rides it, urgent threshold events use FrameLaneAck /
	// FrameLaneGauge. Lack declares this link supports LaneAck reporting;
	// WantTx/WantGauge declare what we want the peer to carry; LaneRx carries
	// our cumulative per-lane rx (only when the peer declared WantTx);
	// Gauge is our congestion gauge scaled 1-255 (only when WantGauge).
	Lack      bool           `json:"lack,omitempty"`
	WantTx    bool           `json:"wantTx,omitempty"`
	WantGauge bool           `json:"wantGauge,omitempty"`
	LaneRx    []GossipLaneRx `json:"laneRx,omitempty"`
	Gauge     int            `json:"gauge,omitempty"`
}

// GossipLaneRx is one lane's cumulative rx bytes in the LaneAck gossip
// fallback (laneID + count; the urgent FrameLaneAck uses a binary encoding).
type GossipLaneRx struct {
	ID uint32 `json:"id"`
	Rx uint64 `json:"rx"`
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
	// Drop any stale entry for the same node that is no longer backed by this
	// sender (reconnect before the old entry was unregistered). Leftovers keep
	// advertising their last known claims — including neighbor lists — and can
	// shadow live data in bestClaims, which shows up as permanently missing
	// topology edges until a restart. nodeID is empty until the first gossip,
	// so only dedupe when it is known.
	if id := sender.GetNodeID(); id != "" {
		kept := t.peers[:0]
		for _, p := range t.peers {
			if p.Sender != sender && p.NodeID() == id {
				continue
			}
			kept = append(kept, p)
		}
		t.peers = kept
	}
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

// UnregisterPeerByNodeID removes a peer entry by nodeID.
//
// Two entries may legitimately share a nodeID (one per link, multi-path to the
// same node), so this is intentionally left as a single-entry removal and has
// no production caller: link teardown always goes through UnregisterPeer with
// the exact Sender. Do not turn this into a bulk delete.
func (t *Topology) UnregisterPeerByNodeID(nodeID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, p := range t.peers {
		if p.NodeID() == nodeID {
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
	idx := -1
	for i, p := range t.peers {
		if p.Sender == sender {
			peer = p
			idx = i
			break
		}
	}
	if peer == nil {
		return false
	}

	// Parse routes (reference owner node, hop count derived from ClaimedSubnets)
	var routes []PeerRouteEntry
	for _, r := range info.Routes {
		_, ipNet, err := net.ParseCIDR(r.Prefix)
		if err != nil {
			continue
		}
		routes = append(routes, PeerRouteEntry{
			Prefix:    ipNet,
			PrefixStr: r.Prefix,
			NodeID:    r.NodeID,
		})
	}

	// Parse domain suffixes (reference owner node, hop count and subnet derived from ClaimedSubnets)
	var domainSuffixes []PeerDomainSuffixEntry
	for _, ds := range info.DomainSuffixes {
		domainSuffixes = append(domainSuffixes, PeerDomainSuffixEntry{
			Suffix: ds.Suffix,
			NodeID: ds.NodeID,
		})
	}

	// Parse claimed subnets (increment hop count, preserve neighbors)
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
			Neighbors: cs.Neighbors,
		})
	}

	// Gossip claim tracing (mesh=debug): shows exactly what a peer told us,
	// including each neighbor's link seqs. Needed to tell "upstream stripped
	// the link" apart from "we dropped it locally".
	if util.GetModuleLogLevel("mesh") <= util.LogLevelDebug {
		util.LogModuleDebug("mesh", "[GOSSIP-IN] from=%s claims=%s",
			sender.GetNodeID(), summarizePeerClaims(claimedSubnets))
	}

	// Derive peer's own subnet from ClaimedSubnets with hop=1 (originally hop=0 from sender)
	var subnet *net.IPNet
	var subnetStr string
	for _, cs := range claimedSubnets {
		if cs.Hop == 1 && cs.NodeID != "" {
			// This is the sender's own subnet (hop was 0, now 1 after increment)
			subnet = cs.Subnet
			subnetStr = cs.SubnetStr
			break
		}
	}

	// Check if anything changed
	changed := false
	if peer.SubnetStr != subnetStr {
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

	// Publish a NEW PeerInfo instead of mutating the existing one in place.
	//
	// Readers (broadcastGossip via the atomic topology snapshot, GetFullTopology,
	// GetPeer) hold *PeerInfo pointers and read fields WITHOUT holding t.mu. With
	// in-place mutation they could observe a half-updated struct — most dangerous
	// for ClaimedSubnets, where a torn/stale slice header yields the previous
	// gossip's neighbor list (e.g. a link that was just added is missing from the
	// claims we forward). Replacing the pointer makes every read see a complete,
	// immutable version.
	t.peers[idx] = &PeerInfo{
		Sender:         peer.Sender,
		Subnet:         subnet,
		SubnetStr:      subnetStr,
		DomainSuffixes: domainSuffixes,
		Routes:         routes,
		ClaimedSubnets: claimedSubnets,
		LastSeen:       time.Now(),
	}

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

// GetPeer returns a peer by nodeID, or nil if not found.
func (t *Topology) GetPeer(nodeID string) *PeerInfo {
	t.mu.RLock()
	defer t.mu.RUnlock()

	for _, peer := range t.peers {
		if peer.NodeID() == nodeID {
			return peer
		}
	}
	return nil
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
		if a[i].PrefixStr != b[i].PrefixStr || a[i].NodeID != b[i].NodeID {
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
		if a[i].Suffix != b[i].Suffix || a[i].NodeID != b[i].NodeID {
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
		// Compare neighbors
		if len(a[i].Neighbors) != len(b[i].Neighbors) {
			return false
		}
		// Compare each neighbor
		for j := range a[i].Neighbors {
			if !compareGossipNeighbors(a[i].Neighbors[j], b[i].Neighbors[j]) {
				return false
			}
		}
	}
	return true
}

// compareGossipNeighbors compares two GossipNeighbor structs for equality.
// Only link identity fields participate: SRTT/LossRate/Jitter change every
// ping cycle and must NOT mark gossip as changed (that would recompute routes
// every cycle via processGossip). Quality updates flow through qualityLoop's
// periodic recompute instead (环路-3).
func compareGossipNeighbors(a, b GossipNeighbor) bool {
	if a.NodeID != b.NodeID {
		return false
	}
	if len(a.Links) != len(b.Links) {
		return false
	}
	// Simple comparison - order matters
	for i := range a.Links {
		if a.Links[i].LocalSeq != b.Links[i].LocalSeq ||
			a.Links[i].RemoteSeq != b.Links[i].RemoteSeq ||
			a.Links[i].FriendlyName != b.Links[i].FriendlyName {
			return false
		}
	}
	return true
}
