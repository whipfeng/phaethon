package mesh

import (
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

// TunInterface abstracts the TUN engine for mesh packet injection.
type TunInterface interface {
	InjectMeshPacket(data []byte) error
	WriteMeshPacket(data []byte) error
}

// PeerSender sends data directly to a connected peer.
type PeerSender interface {
	Send(data []byte) error
	GetNodeID() string
}

// P2PTransport abstracts the P2P layer for mesh packet delivery.
type P2PTransport interface {
	BroadcastMeshGossip(data []byte) error
	ListMeshPeerIDs() []string
	SendMeshDNSQuery(peerNodeID string, domain string, queryID uint16) error
}

// MeshPeerInfo describes a connected mesh peer.
type MeshPeerInfo struct {
	NodeID   string    `json:"nodeId"`
	Subnet   string    `json:"subnet"`
	Direct   bool      `json:"direct"`
	LastSeen time.Time `json:"lastSeen"`
}

// MeshRoute represents a route to a network prefix via a peer.
type MeshRoute struct {
	Prefix *net.IPNet
	Peer   PeerSender
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

	routesMu sync.RWMutex
	routes   []MeshRoute // sorted by prefix length (longest first)
	domainTrie *DomainTrie

	dnsPendingMu sync.Mutex
	dnsPending   map[uint16]chan dnsResult
	dnsQueryID   uint16

	DNSAllocator func(domain string) (net.IP, error)

	natTable *NATTable
	closeCh  chan struct{}
}

type dnsResult struct {
	fakeIP net.IP
	err    error
}

func NewMeshManager(nodeID string, vip net.IP, additionalVIPs []net.IP, subnet *net.IPNet, subnetStr string, domainSuffixes []string, advertise []string) *MeshManager {
	return &MeshManager{
		nodeID:         nodeID,
		vip:            vip.To4(),
		subnet:         subnet,
		subnetStr:      subnetStr,
		domainSuffixes: domainSuffixes,
		advertise:      advertise,
		topology:       NewTopology(),
		routes:         make([]MeshRoute, 0),
		dnsPending:     make(map[uint16]chan dnsResult),
		closeCh:        make(chan struct{}),
	}
}

func (m *MeshManager) GetVIP() net.IP {
	return m.vip
}

func (m *MeshManager) GetAllVIPs() []net.IP {
	return []net.IP{m.vip}
}

func (m *MeshManager) GetSubnet() string {
	return m.subnetStr
}

func (m *MeshManager) EnableNAT() {
	m.natTable = NewNATTable(m.vip)
	util.LogInfo("[MESH] NAT enabled (vip=%s)", m.vip)
}

func (m *MeshManager) GetNATStats() int {
	if m.natTable == nil {
		return 0
	}
	return m.natTable.Stats()
}

func (m *MeshManager) GetDomainSuffixes() []string {
	return m.domainSuffixes
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

// isLocalNetstackAddr checks if the IP is a local netstack address (GIP .3 or hostIP .2).
// These addresses are handled by the netstack internally via InjectInbound, not by mesh routing.
func (m *MeshManager) isLocalNetstackAddr(ip net.IP) bool {
	if m.subnet == nil {
		return false
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	baseIP := m.subnet.IP.To4()
	if baseIP == nil {
		return false
	}
	// .2 = hostIP (TUN adapter OS side)
	// .3 = GIP (netstack internal, DNS)
	hostIP := net.IP{baseIP[0], baseIP[1], baseIP[2], baseIP[3] + 2}
	gip := net.IP{baseIP[0], baseIP[1], baseIP[2], baseIP[3] + 3}
	return ip4.Equal(hostIP) || ip4.Equal(gip)
}

func (m *MeshManager) SetTun(tun TunInterface) {
	m.tun = tun
}

var GlobalMeshManager *MeshManager

func (m *MeshManager) Start(tun TunInterface, p2p P2PTransport) {
	m.tun = tun
	m.p2p = p2p
	m.recomputeRoutes()
	go m.gossipLoop()
	util.LogInfo("[MESH] started: nodeID=%s vip=%s subnet=%s", m.nodeID, m.vip, m.subnetStr)
}

func (m *MeshManager) Stop() {
	close(m.closeCh)
}

func (m *MeshManager) HandleMeshDNSQuery(fromNodeID string, domain string, queryID uint16) (net.IP, error) {
	util.LogInfo("[MESH] DNS query from %s: %s (queryID=%d)", fromNodeID, domain, queryID)
	if m.DNSAllocator == nil {
		return nil, fmt.Errorf("mesh DNS: no allocator configured")
	}
	fakeIP, err := m.DNSAllocator(domain)
	if err != nil {
		return nil, fmt.Errorf("mesh DNS: %w", err)
	}
	util.LogInfo("[MESH] DNS allocated: %s -> %s for %s", domain, fakeIP, fromNodeID)
	return fakeIP, nil
}

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

func (m *MeshManager) MeshDNSForwarder(domain string) (net.IP, error) {
	gatewayNodeID, _, _ := m.FindGatewayForDomain(domain)
	if gatewayNodeID == "" || gatewayNodeID == m.nodeID {
		return nil, nil
	}
	util.LogInfo("[MESH] DNS forward: %s -> gateway %s", domain, gatewayNodeID)
	return m.ForwardDNSQuery(domain, gatewayNodeID)
}

// RegisterPeer is called when a P2P peer with mesh capability connects.
func (m *MeshManager) RegisterPeer(sender PeerSender) {
	m.topology.RegisterSender(sender)
	util.LogInfo("[MESH] peer registered: %s", sender.GetNodeID())
}

// UnregisterPeer is called when a P2P peer disconnects.
func (m *MeshManager) UnregisterPeer(peerNodeID string) {
	m.topology.RemovePeer(peerNodeID)
	m.recomputeRoutes()
	util.LogInfo("[MESH] peer unregistered: %s", peerNodeID)
}

// HandleOutboundPacket is the TUN readLoop interceptor.
// Returns true if the packet was handled.
func (m *MeshManager) HandleOutboundPacket(dstIP net.IP, data []byte) bool {
	// Exclude local netstack addresses (GIP .3, hostIP .2) from mesh interception.
	// These packets must reach InjectInbound so the netstack's DNS hijacker can process them.
	if m.isLocalNetstackAddr(dstIP) {
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

	// Exclude mesh subnet (Fake-IPs) from mesh interception.
	// Fake-IPs are allocated from the mesh subnet but are not actual VIPs.
	// They must reach InjectInbound so the gVisor TCP forwarder can handle them
	// and look up the original domain via fakeIP.LookupDomain.
	if m.subnet != nil && m.subnet.Contains(dstIP) {
		return false
	}

	peer := m.findPeer(dstIP)
	if peer == nil {
		return false
	}

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
		if err := peer.Send(pkt); err != nil {
			util.LogWarn("[MESH] send to %s failed: %v", peer.GetNodeID(), err)
		}
	}()
	return true
}

// HandleMeshFrame processes a raw IP packet received from a peer.
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

	if m.isLocalVIP(dstIP) {
		pkt := make([]byte, len(frame))
		copy(pkt, frame)
		if m.natTable != nil {
			natPkt := m.natTable.TranslateInbound(pkt)
			if natPkt != nil {
				pkt = natPkt
			}
		}
		go func() {
			if m.tun != nil {
				if err := m.tun.WriteMeshPacket(pkt); err != nil {
					util.LogWarn("[MESH] write to TUN failed: %v", err)
				}
			}
		}()
		return
	}

	if frame[8] <= 1 {
		return
	}

	peer := m.findPeer(dstIP)
	if peer == nil {
		// No mesh route — we're the gateway for this destination.
		// Inject into local netstack so it goes out via proxy/direct.
		pkt := make([]byte, len(frame))
		copy(pkt, frame)
		go func() {
			if m.tun != nil {
				if err := m.tun.InjectMeshPacket(pkt); err != nil {
					util.LogWarn("[MESH] inject to local netstack failed: %v", err)
				}
			}
		}()
		return
	}

	pkt := make([]byte, len(frame))
	copy(pkt, frame)
	decrementIPTTL(pkt)
	go func() {
		if err := peer.Send(pkt); err != nil {
			util.LogWarn("[MESH] forward to %s failed: %v", peer.GetNodeID(), err)
		}
	}()
}

// HandleTopologyGossip processes a gossip announcement from a peer.
func (m *MeshManager) HandleTopologyGossip(fromNodeID string, data []byte) {
	var info GossipInfo
	if err := json.Unmarshal(data, &info); err != nil {
		util.LogDebug("[MESH] bad gossip from %s: %v", fromNodeID, err)
		return
	}
	if info.NodeID == m.nodeID {
		return
	}
	util.LogDebug("[MESH] gossip from %s: subnet=%s routes=%v", fromNodeID, info.Subnet, info.Routes)
	if m.topology.UpdateFromGossip(info) {
		m.recomputeRoutes()
	}
}

func (m *MeshManager) GetStatus() map[string]interface{} {
	m.routesMu.RLock()
	routeCount := len(m.routes)
	m.routesMu.RUnlock()

	return map[string]interface{}{
		"enabled":        true,
		"nodeId":         m.nodeID,
		"vip":            m.vip.String(),
		"subnet":         m.subnetStr,
		"domainSuffixes": m.domainSuffixes,
		"advertise":      m.advertise,
		"routeCount":     routeCount,
	}
}

func (m *MeshManager) GetTopology() map[string]interface{} {
	peers := m.topology.GetAllPeers()
	peerList := make([]map[string]interface{}, 0, len(peers))
	for _, p := range peers {
		entry := map[string]interface{}{
			"nodeId":   p.NodeID,
			"subnet":   p.SubnetStr,
			"lastSeen": p.LastSeen,
		}
		if len(p.DomainSuffixes) > 0 {
			entry["domainSuffixes"] = p.DomainSuffixes
		}
		if len(p.Routes) > 0 {
			routeStrs := make([]string, 0, len(p.Routes))
			for _, r := range p.Routes {
				routeStrs = append(routeStrs, r.String())
			}
			entry["routes"] = routeStrs
		}
		peerList = append(peerList, entry)
	}
	return map[string]interface{}{"peers": peerList}
}

func (m *MeshManager) GetRoutes() map[string]interface{} {
	m.routesMu.RLock()
	defer m.routesMu.RUnlock()

	routeList := make([]map[string]interface{}, 0, len(m.routes))
	for _, r := range m.routes {
		routeList = append(routeList, map[string]interface{}{
			"prefix": r.Prefix.String(),
			"via":    r.Peer.GetNodeID(),
		})
	}
	return map[string]interface{}{"routes": routeList}
}

func (m *MeshManager) GetPeers() []MeshPeerInfo {
	if m.p2p == nil {
		return nil
	}
	ids := m.p2p.ListMeshPeerIDs()
	result := make([]MeshPeerInfo, 0, len(ids))
	for _, id := range ids {
		info := MeshPeerInfo{NodeID: id, Direct: true}
		p := m.topology.GetPeer(id)
		if p != nil {
			info.Subnet = p.SubnetStr
			info.LastSeen = p.LastSeen
		}
		result = append(result, info)
	}
	return result
}

func (m *MeshManager) ResolveMeshDomain(domain string) net.IP {
	nodeID := ParseNodeDomain(domain)
	if nodeID == "" {
		return nil
	}
	peer := m.topology.GetPeer(nodeID)
	if peer == nil || peer.Subnet == nil {
		return nil
	}
	vip := DeriveVIPFromSubnet(peer.Subnet)
	if vip != nil {
		util.LogInfo("[MESH] DNS resolve: %s -> %s", domain, vip)
	}
	return vip
}

func (m *MeshManager) recomputeRoutes() {
	peers := m.topology.GetAllPeers()
	util.LogDebug("[MESH] recomputeRoutes: %d peers", len(peers))

	var routes []MeshRoute

	for _, peer := range peers {
		if peer.NodeID == m.nodeID {
			continue
		}
		if peer.Sender == nil {
			continue
		}
		// Add subnet as a route
		if peer.Subnet != nil {
			routes = append(routes, MeshRoute{
				Prefix: peer.Subnet,
				Peer:   peer.Sender,
			})
		}
		// Add advertised routes
		for _, r := range peer.Routes {
			routes = append(routes, MeshRoute{
				Prefix: r,
				Peer:   peer.Sender,
			})
		}
	}

	// Sort by prefix length (longest first for longest prefix match)
	sort.Slice(routes, func(i, j int) bool {
		lenI, _ := routes[i].Prefix.Mask.Size()
		lenJ, _ := routes[j].Prefix.Mask.Size()
		return lenI > lenJ
	})

	domainTrie := BuildFromTopology(m.topology)

	m.routesMu.Lock()
	m.routes = routes
	m.domainTrie = domainTrie
	m.routesMu.Unlock()
	util.LogInfo("[MESH] routes recomputed: %d routes", len(routes))
	for _, r := range routes {
		util.LogInfo("[MESH]   %s -> %s", r.Prefix, r.Peer.GetNodeID())
	}
}

// findPeer finds the peer for a destination IP using longest prefix match.
func (m *MeshManager) findPeer(dstIP net.IP) PeerSender {
	m.routesMu.RLock()
	defer m.routesMu.RUnlock()

	for _, route := range m.routes {
		if route.Prefix.Contains(dstIP) {
			return route.Peer
		}
	}
	return nil
}

func (m *MeshManager) FindGatewayForDomain(domain string) (string, string, int) {
	m.routesMu.RLock()
	trie := m.domainTrie
	m.routesMu.RUnlock()

	if trie == nil {
		return "", "", 0
	}
	nodeID, suffixLen := trie.Lookup(domain)
	if nodeID == "" {
		return "", "", 0
	}
	return nodeID, nodeID, suffixLen
}

func (m *MeshManager) gossipLoop() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.closeCh:
			return
		case <-ticker.C:
			info := GossipInfo{
				NodeID:         m.nodeID,
				Subnet:         m.subnetStr,
				DomainSuffixes: m.domainSuffixes,
				Routes:         m.advertise,
			}
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
