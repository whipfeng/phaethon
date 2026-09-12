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
	SendMeshGossipTo(peerNodeID string, data []byte) error
	ListMeshPeerIDs() []string
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

	DNSAllocator func(domain string) (net.IP, error)

	natTable *NATTable
	closeCh  chan struct{}
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

// getHostIP returns the hostIP (.2) address for this node's subnet.
// hostIP is the TUN interface address on the OS side.
func (m *MeshManager) getHostIP() net.IP {
	if m.subnet == nil {
		return nil
	}
	baseIP := m.subnet.IP.To4()
	if baseIP == nil {
		return nil
	}
	return net.IP{baseIP[0], baseIP[1], baseIP[2], baseIP[3] + 2}
}

// getGIP returns the GIP (.3) address for this node's subnet.
// GIP is the DNS service address, delivered to gVisor netstack.
func (m *MeshManager) getGIP() net.IP {
	if m.subnet == nil {
		return nil
	}
	baseIP := m.subnet.IP.To4()
	if baseIP == nil {
		return nil
	}
	return net.IP{baseIP[0], baseIP[1], baseIP[2], baseIP[3] + 3}
}

// isLocalHostIP checks if the IP is the local hostIP (.2) address.
func (m *MeshManager) isLocalHostIP(ip net.IP) bool {
	hostIP := m.getHostIP()
	return hostIP != nil && ip.Equal(hostIP)
}

// isLocalGIP checks if the IP is the local GIP (.3) address.
func (m *MeshManager) isLocalGIP(ip net.IP) bool {
	gip := m.getGIP()
	return gip != nil && ip.Equal(gip)
}

// isLocalNetstackAddr checks if the IP is a local netstack address (GIP .3 or hostIP .2).
// These addresses are handled by the netstack internally via InjectInbound, not by mesh routing.
func (m *MeshManager) isLocalNetstackAddr(ip net.IP) bool {
	return m.isLocalHostIP(ip) || m.isLocalGIP(ip)
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

// GetGatewayGIPForDomain returns the GIP (.3) address of the gateway for a domain.
// Returns nil if no gateway is found or the gateway is ourselves.
func (m *MeshManager) GetGatewayGIPForDomain(domain string) net.IP {
	gatewayNodeID, _, _ := m.FindGatewayForDomain(domain)
	if gatewayNodeID == "" || gatewayNodeID == m.nodeID {
		return nil
	}
	peer := m.topology.GetPeer(gatewayNodeID)
	if peer == nil || peer.Subnet == nil {
		return nil
	}
	return DeriveGIPFromSubnet(peer.Subnet)
}

// DNSNetstackForwarder is a callback that forwards DNS queries via netstack socket.
// It takes the domain and gateway GIP, and returns the fakeIP from the response.
type DNSNetstackForwarder func(domain string, gatewayGIP net.IP) (net.IP, error)

// dnsNetstackForwarder is the callback for forwarding DNS via netstack.
var dnsNetstackForwarder DNSNetstackForwarder

// SetDNSNetstackForwarder sets the callback for forwarding DNS via netstack socket.
func SetDNSNetstackForwarder(f DNSNetstackForwarder) {
	dnsNetstackForwarder = f
}

// ForwardDNSViaNetstack forwards a DNS query to the gateway's GIP:53 via netstack socket.
// Returns the fakeIP from the gateway's response, or (nil, nil) if no gateway is found.
func (m *MeshManager) ForwardDNSViaNetstack(domain string) (net.IP, error) {
	gatewayGIP := m.GetGatewayGIPForDomain(domain)
	if gatewayGIP == nil {
		return nil, nil
	}
	if dnsNetstackForwarder == nil {
		return nil, fmt.Errorf("DNS netstack forwarder not set")
	}
	util.LogInfo("[MESH] DNS netstack forward: %s -> gateway GIP %s", domain, gatewayGIP)
	return dnsNetstackForwarder(domain, gatewayGIP)
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
		if dstIP[0] == 100 && dstIP[1] == 64 {
			util.LogInfo("[MESH] outbound %s: local subnet, passing through", dstIP)
		}
		return false
	}

	peer := m.findPeer(dstIP)
	if peer == nil {
		if dstIP[0] == 100 && dstIP[1] == 64 {
			util.LogInfo("[MESH] outbound %s: no peer found, passing through", dstIP)
		}
		return false
	}

	if dstIP[0] == 100 && dstIP[1] == 64 {
		util.LogInfo("[MESH] outbound %s: sending via peer %s", dstIP, peer.GetNodeID())
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

	if dstIP[0] == 100 && dstIP[1] == 64 {
		util.LogInfo("[MESH] recv frame from %s: dst=%s TTL=%d len=%d", fromNodeID, dstIP, frame[8], len(frame))
	}

	// .1 (VIP): NAT reverse + WriteMeshPacket to OS
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
					util.LogWarn("[MESH] write VIP packet to TUN failed: %v", err)
				}
			}
		}()
		return
	}

	// .2 (hostIP): WriteMeshPacket to OS (deliver to application)
	if m.isLocalHostIP(dstIP) {
		pkt := make([]byte, len(frame))
		copy(pkt, frame)
		go func() {
			if m.tun != nil {
				if err := m.tun.WriteMeshPacket(pkt); err != nil {
					util.LogWarn("[MESH] write hostIP packet to TUN failed: %v", err)
				}
			}
		}()
		return
	}

	// .3 (GIP): InjectMeshPacket to netstack (DNS hijacker)
	if m.isLocalGIP(dstIP) {
		pkt := make([]byte, len(frame))
		copy(pkt, frame)
		go func() {
			if m.tun != nil {
				if err := m.tun.InjectMeshPacket(pkt); err != nil {
					util.LogWarn("[MESH] inject GIP packet to netstack failed: %v", err)
				}
			}
		}()
		return
	}

	if frame[8] <= 1 {
		if dstIP[0] == 100 && dstIP[1] == 64 {
			util.LogInfo("[MESH] recv frame from %s: dst=%s TTL=%d dropped (TTL<=1)", fromNodeID, dstIP, frame[8])
		}
		return
	}

	peer := m.findPeer(dstIP)
	if peer == nil {
		// No mesh route — we're the gateway for this destination.
		// Inject into local netstack so it goes out via proxy/direct.
		if dstIP[0] == 100 && dstIP[1] == 64 {
			util.LogInfo("[MESH] recv frame from %s: dst=%s injecting to local netstack", fromNodeID, dstIP)
		}
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

	if dstIP[0] == 100 && dstIP[1] == 64 {
		util.LogInfo("[MESH] recv frame from %s: dst=%s forwarding to %s", fromNodeID, dstIP, peer.GetNodeID())
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
	// Filter out routes where source is ourselves (prevent echo)
	filtered := info.Routes[:0]
	for _, r := range info.Routes {
		if r.SourceNodeID != m.nodeID {
			filtered = append(filtered, r)
		}
	}
	info.Routes = filtered
	// Filter out domain suffixes where source is ourselves
	filteredDS := info.DomainSuffixes[:0]
	for _, ds := range info.DomainSuffixes {
		if ds.SourceNodeID != m.nodeID {
			filteredDS = append(filteredDS, ds)
		}
	}
	info.DomainSuffixes = filteredDS
	util.LogDebug("[MESH] gossip from %s: subnet=%s routes=%d domainSuffixes=%d", fromNodeID, info.Subnet, len(info.Routes), len(info.DomainSuffixes))
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
			routeEntries := make([]map[string]string, 0, len(p.Routes))
			for _, r := range p.Routes {
				routeEntries = append(routeEntries, map[string]string{
					"prefix":       r.PrefixStr,
					"sourceNodeId": r.SourceNodeID,
				})
			}
			entry["routes"] = routeEntries
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

	type candidate struct {
		sender PeerSender
		prefix *net.IPNet
		bits   int
	}
	best := make(map[string]candidate)

	for _, peer := range peers {
		if peer.NodeID == m.nodeID || peer.Sender == nil {
			continue
		}
		// Peer's own subnet
		if peer.Subnet != nil {
			bits, _ := peer.Subnet.Mask.Size()
			key := peer.Subnet.String()
			if existing, ok := best[key]; !ok || bits > existing.bits {
				best[key] = candidate{peer.Sender, peer.Subnet, bits}
			}
		}
		// Routes synced from this peer
		for _, r := range peer.Routes {
			bits, _ := r.Prefix.Mask.Size()
			if existing, ok := best[r.PrefixStr]; !ok || bits > existing.bits {
				best[r.PrefixStr] = candidate{peer.Sender, r.Prefix, bits}
			}
		}
	}

	routes := make([]MeshRoute, 0, len(best))
	for _, c := range best {
		routes = append(routes, MeshRoute{Prefix: c.prefix, Peer: c.sender})
	}

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
			m.broadcastGossip()
		}
	}
}

func (m *MeshManager) broadcastGossip() {
	if m.p2p == nil {
		return
	}
	peerIDs := m.p2p.ListMeshPeerIDs()
	if len(peerIDs) == 0 {
		return
	}

	// Own routes: subnet + configured advertise, all with src=self
	ownRoutes := []GossipRoute{{SourceNodeID: m.nodeID, Prefix: m.subnetStr}}
	for _, r := range m.advertise {
		ownRoutes = append(ownRoutes, GossipRoute{SourceNodeID: m.nodeID, Prefix: r})
	}

	// Own domain suffixes with src=self
	ownDS := make([]GossipDomainSuffix, 0, len(m.domainSuffixes))
	for _, s := range m.domainSuffixes {
		ownDS = append(ownDS, GossipDomainSuffix{SourceNodeID: m.nodeID, Suffix: s})
	}

	for _, peerID := range peerIDs {
		// Split horizon: exclude routes from this recipient
		learnedRoutes := m.topology.CollectRoutesForGossip(peerID)
		// Split horizon: exclude domain suffixes from this recipient
		learnedDS := m.topology.CollectDomainSuffixesForGossip(peerID)

		allRoutes := make([]GossipRoute, 0, len(ownRoutes)+len(learnedRoutes))
		allRoutes = append(allRoutes, ownRoutes...)
		allRoutes = append(allRoutes, learnedRoutes...)

		allDS := make([]GossipDomainSuffix, 0, len(ownDS)+len(learnedDS))
		allDS = append(allDS, ownDS...)
		allDS = append(allDS, learnedDS...)

		info := GossipInfo{
			NodeID:         m.nodeID,
			Subnet:         m.subnetStr,
			DomainSuffixes: allDS,
			Routes:         allRoutes,
		}
		data, err := json.Marshal(info)
		if err != nil {
			continue
		}
		m.p2p.SendMeshGossipTo(peerID, data)
	}
}
