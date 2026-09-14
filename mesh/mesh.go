package mesh

import (
	"encoding/json"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"phaethon/tun"
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
	SetMeshInfo(nodeID, vip string)
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
	dataDir        string // for state file persistence

	routesMu sync.RWMutex
	routes   []MeshRoute // sorted by prefix length (longest first)
	domainTrie *DomainTrie

	DNSAllocator func(domain string) (net.IP, error)

	natTable *tun.NATTable
	closeCh  chan struct{}
	eventCh  chan meshEvent
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
		eventCh:        make(chan meshEvent, 64),
	}
}

// SetDataDir sets the data directory for state file persistence.
func (m *MeshManager) SetDataDir(dataDir string) {
	m.dataDir = dataDir
}

// checkSubnetConflict checks if our subnet conflicts with any claimed subnet.
// If our nodeId is numerically smaller, we need to re-select.
// Returns true if we re-selected.
func (m *MeshManager) checkSubnetConflict() bool {
	// Collect all claimed subnets from topology
	usedSubnets := make(map[string]bool)
	for _, peer := range m.topology.GetAllPeers() {
		// Peer's own subnet
		if peer.SubnetStr != "" && peer.SubnetStr != m.subnetStr {
			usedSubnets[peer.SubnetStr] = true
		}
		// Peer's learned claims
		for _, cs := range peer.ClaimedSubnets {
			if cs.SubnetStr != m.subnetStr {
				usedSubnets[cs.SubnetStr] = true
			}
		}
	}

	// Check if our subnet is claimed by someone else
	for _, peer := range m.topology.GetAllPeers() {
		// Check peer's own subnet
		if peer.SubnetStr == m.subnetStr && peer.NodeID() != m.nodeID {
			// Conflict! Compare nodeIds numerically
			if compareNodeIDs(m.nodeID, peer.NodeID()) < 0 {
				// Our nodeId is smaller, we need to re-select
				return m.reselectSubnet(usedSubnets)
			}
			// Otherwise, they will re-select
		}
		// Check peer's learned claims
		for _, cs := range peer.ClaimedSubnets {
			if cs.SubnetStr == m.subnetStr && cs.NodeID != m.nodeID {
				if compareNodeIDs(m.nodeID, cs.NodeID) < 0 {
					return m.reselectSubnet(usedSubnets)
				}
			}
		}
	}
	return false
}

// reselectSubnet picks a new subnet that doesn't conflict.
func (m *MeshManager) reselectSubnet(usedSubnets map[string]bool) bool {
	// Mark our current subnet as used so we don't pick it again
	usedSubnets[m.subnetStr] = true

	newSubnetStr, err := AllocateSubnet(usedSubnets)
	if err != nil {
		util.LogError("[MESH] subnet conflict: failed to allocate new subnet: %v", err)
		return false
	}

	_, newSubnet, err := net.ParseCIDR(newSubnetStr)
	if err != nil {
		util.LogError("[MESH] subnet conflict: invalid new subnet %s: %v", newSubnetStr, err)
		return false
	}

	m.mu.Lock()
	m.subnet = newSubnet
	m.subnetStr = newSubnetStr
	m.vip = DeriveVIPFromSubnet(newSubnet)
	m.mu.Unlock()

	util.LogInfo("[MESH] subnet conflict: re-selected subnet to %s (vip=%s)", newSubnetStr, m.vip)

	// Update P2P layer with new VIP
	if m.p2p != nil {
		m.p2p.SetMeshInfo(m.nodeID, m.vip.String())
	}

	// Save state
	if m.dataDir != "" {
		state := &MeshState{
			NodeID: m.nodeID,
			Subnet: newSubnetStr,
		}
		if err := SaveState(m.dataDir, state); err != nil {
			util.LogError("[MESH] failed to save state after subnet re-selection: %v", err)
		}
	}

	return true
}

// compareNodeIDs compares two nodeID strings numerically.
// Returns -1 if a < b, 0 if a == b, 1 if a > b.
func compareNodeIDs(a, b string) int {
	aNum, aErr := strconv.ParseUint(a, 10, 64)
	bNum, bErr := strconv.ParseUint(b, 10, 64)
	if aErr != nil || bErr != nil {
		// Fall back to string comparison if not numeric
		if a < b {
			return -1
		} else if a > b {
			return 1
		}
		return 0
	}
	if aNum < bNum {
		return -1
	} else if aNum > bNum {
		return 1
	}
	return 0
}

func (m *MeshManager) GetVIP() net.IP {
	return m.vip
}

func (m *MeshManager) GetNodeID() string {
	return m.nodeID
}

func (m *MeshManager) GetAllVIPs() []net.IP {
	return []net.IP{m.vip}
}

func (m *MeshManager) GetSubnet() string {
	return m.subnetStr
}

func (m *MeshManager) EnableNAT() {
	m.natTable = tun.NewNATTable(m.vip)
	util.LogInfo("[MESH] NAT enabled (vip=%s)", m.vip)
}

func (m *MeshManager) GetNATStats() int {
	if m.natTable == nil {
		return 0
	}
	return m.natTable.Stats()
}

func (m *MeshManager) GetNATTable() *tun.NATTable {
	return m.natTable
}

func (m *MeshManager) GetDomainSuffixes() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]string, len(m.domainSuffixes))
	copy(result, m.domainSuffixes)
	return result
}

// UpdateConfig hot-swaps domainSuffixes and advertise without restarting.
func (m *MeshManager) UpdateConfig(domainSuffixes, advertise []string) {
	m.mu.Lock()
	m.domainSuffixes = domainSuffixes
	m.advertise = advertise
	m.mu.Unlock()
	m.eventCh <- meshEvent{kind: meshEventConfigUpdate}
}

// TriggerGossip sends an immediate gossip broadcast.
func (m *MeshManager) TriggerGossip() {
	m.eventCh <- meshEvent{kind: meshEventTick}
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
	m.routesMu.RLock()
	trie := m.domainTrie
	m.routesMu.RUnlock()
	if trie == nil {
		return nil
	}

	nextHop, _ := trie.Lookup(domain)
	if nextHop == nil {
		return nil // own entry or no match
	}

	// Derive GIP from next-hop peer's subnet
	return DeriveGIPFromSubnet(nextHop.Subnet)
}

// ResolveGatewayGIP returns the GIP of the remote gateway that serves the given domain.
// Returns nil if no gateway is found or the gateway is ourselves.
func (m *MeshManager) ResolveGatewayGIP(domain string) net.IP {
	return m.GetGatewayGIPForDomain(domain)
}

// RegisterPeer is called when a P2P peer with mesh capability connects.
func (m *MeshManager) RegisterPeer(sender PeerSender) {
	m.eventCh <- meshEvent{kind: meshEventRegister, sender: sender}
}

// UnregisterPeer is called when a P2P peer disconnects.
func (m *MeshManager) UnregisterPeer(sender PeerSender) {
	m.eventCh <- meshEvent{kind: meshEventUnregister, sender: sender}
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

	// .1 (VIP): NAT reverse + src rewrite to local GIP + WriteMeshPacket to OS
	if m.isLocalVIP(dstIP) {
		pkt := make([]byte, len(frame))
		copy(pkt, frame)
		if m.natTable != nil {
			localGIP := m.getGIP()
			if localGIP != nil {
				natPkt := m.natTable.TranslateInboundWithSrc(pkt, localGIP)
				if natPkt != nil {
					pkt = natPkt
				}
			} else {
				natPkt := m.natTable.TranslateInbound(pkt)
				if natPkt != nil {
					pkt = natPkt
				}
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
func (m *MeshManager) HandleTopologyGossip(sender PeerSender, data []byte) {
	var info GossipInfo
	if err := json.Unmarshal(data, &info); err != nil {
		util.LogDebug("[MESH] bad gossip from %s: %v", sender.GetNodeID(), err)
		return
	}
	if info.NodeID == m.nodeID {
		return
	}
	m.eventCh <- meshEvent{kind: meshEventGossip, sender: sender, data: data}
}

func (m *MeshManager) GetStatus() map[string]interface{} {
	m.routesMu.RLock()
	routeCount := len(m.routes)
	m.routesMu.RUnlock()

	m.mu.RLock()
	domainSuffixes := make([]string, len(m.domainSuffixes))
	copy(domainSuffixes, m.domainSuffixes)
	advertise := make([]string, len(m.advertise))
	copy(advertise, m.advertise)
	m.mu.RUnlock()

	return map[string]interface{}{
		"enabled":        true,
		"nodeId":         m.nodeID,
		"vip":            m.vip.String(),
		"subnet":         m.subnetStr,
		"domainSuffixes": domainSuffixes,
		"advertise":      advertise,
		"routeCount":     routeCount,
	}
}

func (m *MeshManager) GetTopology() map[string]interface{} {
	peers := m.topology.GetAllPeers()
	peerList := make([]map[string]interface{}, 0, len(peers))
	for _, p := range peers {
		entry := map[string]interface{}{
			"nodeId":   p.NodeID(),
			"subnet":   p.SubnetStr,
			"lastSeen": p.LastSeen,
		}
		if len(p.DomainSuffixes) > 0 {
			entry["domainSuffixes"] = p.DomainSuffixes
		}
		if len(p.Routes) > 0 {
			routeEntries := make([]map[string]interface{}, 0, len(p.Routes))
			for _, r := range p.Routes {
				routeEntries = append(routeEntries, map[string]interface{}{
					"prefix": r.PrefixStr,
					"hop":    r.Hop,
				})
			}
			entry["routes"] = routeEntries
		}
		if len(p.ClaimedSubnets) > 0 {
			claimEntries := make([]map[string]interface{}, 0, len(p.ClaimedSubnets))
			for _, cs := range p.ClaimedSubnets {
				claimEntries = append(claimEntries, map[string]interface{}{
					"subnet": cs.SubnetStr,
					"nodeId": cs.NodeID,
					"hop":    cs.Hop,
				})
			}
			entry["claimedSubnets"] = claimEntries
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
	allPeers := m.topology.GetAllPeers()
	result := make([]MeshPeerInfo, 0, len(ids))
	for _, id := range ids {
		info := MeshPeerInfo{NodeID: id, Direct: true}
		for _, p := range allPeers {
			if p.NodeID() == id {
				info.Subnet = p.SubnetStr
				info.LastSeen = p.LastSeen
				break
			}
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
	for _, peer := range m.topology.GetAllPeers() {
		if peer.NodeID() == nodeID && peer.Subnet != nil {
			vip := DeriveVIPFromSubnet(peer.Subnet)
			if vip != nil {
				util.LogInfo("[MESH] DNS resolve: %s -> %s", domain, vip)
			}
			return vip
		}
	}
	return nil
}

func (m *MeshManager) recomputeRoutes() {
	m.mu.RLock()
	advertise := make([]string, len(m.advertise))
	copy(advertise, m.advertise)
	domainSuffixes := make([]string, len(m.domainSuffixes))
	copy(domainSuffixes, m.domainSuffixes)
	m.mu.RUnlock()

	peers := m.topology.GetAllPeers()

	// Build global route table: prefix → {hop, nextHop peer}
	type globalEntry struct {
		hop     int
		nextHop *PeerInfo // nil = own route
		prefix  *net.IPNet
	}
	best := make(map[string]globalEntry)

	// Own advertise routes (Hop=0, NextHop=nil) — non-mesh routes
	for _, r := range advertise {
		_, ipNet, err := net.ParseCIDR(r)
		if err != nil {
			continue
		}
		best[r] = globalEntry{0, nil, ipNet}
	}

	// Peer routes (non-mesh, with hop counts)
	for _, peer := range peers {
		if peer.Sender == nil {
			continue
		}
		for _, r := range peer.Routes {
			if existing, ok := best[r.PrefixStr]; !ok || r.Hop < existing.hop {
				best[r.PrefixStr] = globalEntry{r.Hop, peer, r.Prefix}
			}
		}
	}

	// Build mesh subnet routes from claimedSubnets
	// Own subnet (Hop=0, NextHop=nil) — not added to routes (own, no forwarding needed)
	// Peer claimed subnets (mesh routing)
	for _, peer := range peers {
		if peer.Sender == nil {
			continue
		}
		// Peer's own subnet claim
		if peer.Subnet != nil {
			key := peer.Subnet.String()
			if existing, ok := best[key]; !ok || 1 < existing.hop {
				best[key] = globalEntry{1, peer, peer.Subnet}
			}
		}
		// Peer's learned claims
		for _, cs := range peer.ClaimedSubnets {
			if existing, ok := best[cs.SubnetStr]; !ok || cs.Hop < existing.hop {
				best[cs.SubnetStr] = globalEntry{cs.Hop, peer, cs.Subnet}
			}
		}
	}

	// Build MeshRoute slice (exclude own routes where nextHop=nil)
	routes := make([]MeshRoute, 0, len(best))
	for _, e := range best {
		if e.nextHop == nil {
			continue // own route, no forwarding needed
		}
		routes = append(routes, MeshRoute{Prefix: e.prefix, Peer: e.nextHop.Sender})
	}

	sort.Slice(routes, func(i, j int) bool {
		lenI, _ := routes[i].Prefix.Mask.Size()
		lenJ, _ := routes[j].Prefix.Mask.Size()
		return lenI > lenJ
	})

	// Build global domain trie
	trie := NewDomainTrie()
	// Own domain suffixes (Hop=0, NextHop=nil)
	for _, s := range domainSuffixes {
		trie.Insert(s, nil, 0)
	}
	// Peer domain suffixes
	for _, peer := range peers {
		if peer.Sender == nil {
			continue
		}
		for _, entry := range peer.DomainSuffixes {
			trie.Insert(entry.Suffix, peer, entry.Hop)
		}
	}

	m.routesMu.Lock()
	m.routes = routes
	m.domainTrie = trie
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

func (m *MeshManager) gossipLoop() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.closeCh:
			return
		case <-ticker.C:
			m.eventCh <- meshEvent{kind: meshEventTick}
		case ev := <-m.eventCh:
			switch ev.kind {
			case meshEventRegister:
				m.topology.RegisterPeer(ev.sender)
				m.recomputeRoutes()
				util.LogInfo("[MESH] peer registered: %s", ev.sender.GetNodeID())
			case meshEventUnregister:
				nodeID := ev.sender.GetNodeID()
				m.topology.UnregisterPeer(ev.sender)
				m.recomputeRoutes()
				util.LogInfo("[MESH] peer unregistered: %s", nodeID)
			case meshEventGossip:
				var info GossipInfo
				if err := json.Unmarshal(ev.data, &info); err != nil {
					util.LogDebug("[MESH] bad gossip from %s: %v", ev.sender.GetNodeID(), err)
					break
				}
				util.LogDebug("[MESH] gossip from %s: subnet=%s routes=%d domainSuffixes=%d claimedSubnets=%d", ev.sender.GetNodeID(), info.Subnet, len(info.Routes), len(info.DomainSuffixes), len(info.ClaimedSubnets))
				if m.topology.UpdateGossip(ev.sender, info) {
					m.recomputeRoutes()
				}
				// Check for subnet conflicts and re-select if needed
				if m.checkSubnetConflict() {
					m.recomputeRoutes()
					m.broadcastGossip()
				}
			case meshEventTick:
				m.broadcastGossip()
			case meshEventConfigUpdate:
				m.recomputeRoutes()
				m.broadcastGossip()
				m.mu.RLock()
				util.LogInfo("[MESH] config updated: domainSuffixes=%v advertise=%v", m.domainSuffixes, m.advertise)
				m.mu.RUnlock()
			}
		}
	}
}

type meshEventKind int

const (
	meshEventRegister meshEventKind = iota
	meshEventUnregister
	meshEventGossip
	meshEventTick
	meshEventConfigUpdate
)

type meshEvent struct {
	kind   meshEventKind
	sender PeerSender
	data   []byte
}

func (m *MeshManager) broadcastGossip() {
	if m.p2p == nil {
		return
	}
	peerIDs := m.p2p.ListMeshPeerIDs()
	if len(peerIDs) == 0 {
		return
	}

	m.mu.RLock()
	advertise := make([]string, len(m.advertise))
	copy(advertise, m.advertise)
	domainSuffixes := make([]string, len(m.domainSuffixes))
	copy(domainSuffixes, m.domainSuffixes)
	m.mu.RUnlock()

	// Capture a single peer snapshot for consistent split-horizon filtering.
	allPeers := m.topology.GetAllPeers()
	peerByNodeID := make(map[string]*PeerInfo, len(allPeers))
	for _, p := range allPeers {
		peerByNodeID[p.NodeID()] = p
	}

	// Build global route table (non-mesh routes only)
	type globalRouteEntry struct {
		hop     int
		nextHop *PeerInfo // nil = own route
		prefix  string
		ipNet   *net.IPNet
	}
	bestRoutes := make(map[string]globalRouteEntry)

	// Own advertise routes (Hop=0) — not mesh subnets
	for _, r := range advertise {
		_, ipNet, _ := net.ParseCIDR(r)
		bestRoutes[r] = globalRouteEntry{0, nil, r, ipNet}
	}
	// Peer routes (non-mesh, learned from peers)
	for _, peer := range allPeers {
		if peer.Sender == nil {
			continue
		}
		for _, r := range peer.Routes {
			if existing, ok := bestRoutes[r.PrefixStr]; !ok || r.Hop < existing.hop {
				bestRoutes[r.PrefixStr] = globalRouteEntry{r.Hop, peer, r.PrefixStr, r.Prefix}
			}
		}
	}

	// Build global claimed subnets table (mesh subnets)
	type globalClaimEntry struct {
		hop     int
		nextHop *PeerInfo // nil = own claim
		nodeID  string
		subnet  string
	}
	bestClaims := make(map[string]globalClaimEntry)

	// Own claim (Hop=0)
	bestClaims[m.subnetStr] = globalClaimEntry{0, nil, m.nodeID, m.subnetStr}
	// Peer claims
	for _, peer := range allPeers {
		if peer.Sender == nil {
			continue
		}
		// Peer's own subnet
		if peer.SubnetStr != "" {
			key := peer.SubnetStr
			if existing, ok := bestClaims[key]; !ok || 1 < existing.hop {
				bestClaims[key] = globalClaimEntry{1, peer, peer.NodeID(), peer.SubnetStr}
			}
		}
		// Peer's learned claims
		for _, cs := range peer.ClaimedSubnets {
			if existing, ok := bestClaims[cs.SubnetStr]; !ok || cs.Hop < existing.hop {
				bestClaims[cs.SubnetStr] = globalClaimEntry{cs.Hop, peer, cs.NodeID, cs.SubnetStr}
			}
		}
	}

	// Build global domain suffix map
	type globalDSEntry struct {
		hop     int
		nextHop *PeerInfo // nil = own entry
	}
	bestDS := make(map[string]globalDSEntry)
	for _, s := range domainSuffixes {
		bestDS[s] = globalDSEntry{0, nil}
	}
	for _, peer := range allPeers {
		if peer.Sender == nil {
			continue
		}
		for _, entry := range peer.DomainSuffixes {
			if existing, ok := bestDS[entry.Suffix]; !ok || entry.Hop < existing.hop {
				bestDS[entry.Suffix] = globalDSEntry{entry.Hop, peer}
			}
		}
	}

	// Per-peer: filter by split horizon and send
	for _, peerID := range peerIDs {
		peer := peerByNodeID[peerID]
		if peer == nil {
			util.LogWarn("[MESH] peer %s not found in topology, skipping gossip", peerID)
			continue
		}

		// Filter routes: exclude entries where nextHop == this peer
		var routes []GossipRoute
		for _, e := range bestRoutes {
			if e.nextHop == peer {
				continue // split horizon
			}
			routes = append(routes, GossipRoute{Prefix: e.prefix, Hop: e.hop})
		}

		// Filter domain suffixes: exclude entries where nextHop == this peer
		var ds []GossipDomainSuffix
		for suffix, e := range bestDS {
			if e.nextHop == peer {
				continue // split horizon
			}
			ds = append(ds, GossipDomainSuffix{Suffix: suffix, Hop: e.hop})
		}

		// Filter claimed subnets: exclude entries where nextHop == this peer
		var claims []GossipClaimedSubnet
		for _, e := range bestClaims {
			if e.nextHop == peer {
				continue // split horizon
			}
			claims = append(claims, GossipClaimedSubnet{
				Subnet: e.subnet,
				NodeID: e.nodeID,
				Hop:    e.hop,
			})
		}

		info := GossipInfo{
			NodeID:         m.nodeID,
			Subnet:         m.subnetStr,
			DomainSuffixes: ds,
			Routes:         routes,
			ClaimedSubnets: claims,
		}
		data, err := json.Marshal(info)
		if err != nil {
			continue
		}
		m.p2p.SendMeshGossipTo(peerID, data)
	}
}
