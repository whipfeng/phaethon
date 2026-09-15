package mesh

import (
	"encoding/json"
	"math/rand"
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

// logTCPPacketMesh logs TCP packet details for debugging
func logTCPPacketMesh(prefix string, data []byte) {
	if len(data) < 20 {
		return
	}
	srcIP := net.IP(data[12:16])
	dstIP := net.IP(data[16:20])
	headerLen := int(data[0]&0x0f) * 4
	if len(data) < headerLen+20 {
		util.LogDebug("%s %s -> %s (TCP header too short)", prefix, srcIP, dstIP)
		return
	}
	srcPort := uint16(data[headerLen])<<8 | uint16(data[headerLen+1])
	dstPort := uint16(data[headerLen+2])<<8 | uint16(data[headerLen+3])
	seq := uint32(data[headerLen+4])<<24 | uint32(data[headerLen+5])<<16 | uint32(data[headerLen+6])<<8 | uint32(data[headerLen+7])
	ack := uint32(data[headerLen+8])<<24 | uint32(data[headerLen+9])<<16 | uint32(data[headerLen+10])<<8 | uint32(data[headerLen+11])
	flags := data[headerLen+13]
	flagStr := ""
	if flags&0x02 != 0 {
		flagStr += "SYN "
	}
	if flags&0x10 != 0 {
		flagStr += "ACK "
	}
	if flags&0x01 != 0 {
		flagStr += "FIN "
	}
	if flags&0x04 != 0 {
		flagStr += "RST "
	}
	util.LogDebug("%s %s:%d -> %s:%d [%s] seq=%d ack=%d len=%d",
		prefix, srcIP, srcPort, dstIP, dstPort, flagStr, seq, ack, len(data))
}

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
	SendGossip(data []byte)
	GetNodeID() string
}

// P2PTransport abstracts the P2P layer for mesh packet delivery.
type P2PTransport interface {
	BroadcastMeshGossip(data []byte) error
	SendMeshGossipTo(peerNodeID string, data []byte) error
	SendMeshGossipToAll(data []byte)
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

	network         *net.IPNet // overall mesh network (e.g., 100.0.0.0/8)
	subnetPrefixLen int        // per-node subnet prefix length (e.g., 16 for /16)

	routesMu sync.RWMutex
	routes   []MeshRoute // sorted by prefix length (longest first)
	domainTrie *DomainTrie

	DNSAllocator func(domain string) (net.IP, error)

	natTable *tun.NATTable
	closeCh  chan struct{}
	eventCh  chan meshEvent
}

func NewMeshManager(nodeID string, vip net.IP, additionalVIPs []net.IP, subnet *net.IPNet, subnetStr string, domainSuffixes []string, advertise []string, network *net.IPNet, subnetPrefixLen int) *MeshManager {
	return &MeshManager{
		nodeID:          nodeID,
		vip:             vip.To4(),
		subnet:          subnet,
		subnetStr:       subnetStr,
		domainSuffixes:  domainSuffixes,
		advertise:       advertise,
		topology:        NewTopology(),
		network:         network,
		subnetPrefixLen: subnetPrefixLen,
		routes:          make([]MeshRoute, 0),
		closeCh:         make(chan struct{}),
		eventCh:         make(chan meshEvent, 64),
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
			// Conflict! Compare nodeIds — larger wins
			if compareNodeIDs(m.nodeID, peer.NodeID()) < 0 {
				// Our nodeId is smaller, we lose — re-select
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

	newSubnetStr, err := AllocateSubnet(m.network, m.subnetPrefixLen, usedSubnets)
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

// compareNodeIDs compares two nodeID strings.
// Returns -1 if a < b, 0 if a == b, 1 if a > b.
// Conflict resolution: larger nodeID wins. Non-numeric strings (e.g. "vm")
// are compared lexicographically and naturally beat numeric strings in ASCII order,
// giving manually named nodes priority over auto-generated ones.
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
	// Set mesh info on P2P layer so hello messages include correct meshNodeId
	p2p.SetMeshInfo(m.nodeID, m.vip.String())
	m.recomputeRoutes()
	go m.gossipLoop()
	util.LogInfo("[MESH] started: nodeID=%s vip=%s subnet=%s subnetStr=%s", m.nodeID, m.vip, m.subnet, m.subnetStr)
	util.LogInfo("[MESH-DEBUG] binary version with subnet logging")
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
	if nextHop.Subnet != nil {
		return DeriveGIPFromSubnet(nextHop.Subnet)
	}

	// Fallback: peer has no subnet (stale connection), find another peer with same nodeID
	targetNodeID := nextHop.NodeID()
	peers := m.topology.GetAllPeers()
	for _, peer := range peers {
		if peer.Sender != nil && peer.Sender.GetNodeID() == targetNodeID && peer.Subnet != nil {
			return DeriveGIPFromSubnet(peer.Subnet)
		}
	}
	return nil
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
	// Debug: log all packets to mesh network
	if isMeshAddress(dstIP) {
		proto := "unknown"
		if len(data) >= 20 && data[0]>>4 == 4 {
			if data[9] == 6 {
				proto = "TCP"
			} else if data[9] == 17 {
				proto = "UDP"
			}
		}
		util.LogInfo("[MESH-DEBUG] HandleOutboundPacket: dst=%s proto=%s len=%d", dstIP, proto, len(data))
	}

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
	//
	// IMPORTANT: Check findPeers FIRST before local subnet check.
	// Remote Fake-IPs (e.g., 100.64.0.x on VM with subnet 100.64.1.0/24) should be
	// routed via the peer that owns that subnet, not passed to local netstack.
	// Load balance across multiple P2P connections to the same node.
	peers := m.findPeers(dstIP)
	if len(peers) > 0 {
		// Select a peer randomly for load balancing
		selectedPeer := peers[rand.Intn(len(peers))]

		// Remote peer owns this IP — send via mesh
		pkt := make([]byte, len(data))
		copy(pkt, data)

		if isMeshAddress(dstIP) {
			util.LogDebug("[MESH] outbound %s: sending %d bytes via peer %s (of %d)", dstIP, len(pkt), selectedPeer.GetNodeID(), len(peers))
			if len(pkt) >= 20 && pkt[9] == 6 {
				logTCPPacketMesh("[TCP-DEBUG] outbound:", pkt)
			}
		}

		go func() {
			if err := selectedPeer.Send(pkt); err != nil {
				util.LogWarn("[MESH] send to %s failed: %v", selectedPeer.GetNodeID(), err)
			} else if len(pkt) >= 20 && pkt[0]>>4 == 4 {
				dst := net.IP(pkt[16:20])
				if isMeshAddress(dst) {
					util.LogDebug("[MESH] sent %d bytes to %s via peer %s OK", len(pkt), dst, selectedPeer.GetNodeID())
				}
			}
		}()
		return true
	}

	// No peer owns this IP — check if it's in our local subnet
	if m.subnet != nil && m.subnet.Contains(dstIP) {
		if isMeshAddress(dstIP) {
			util.LogDebug("[MESH] outbound %s: local subnet %s, passing through", dstIP, m.subnetStr)
		}
		return false
	}

	// Not in local subnet and no peer found — pass through
	if isMeshAddress(dstIP) {
		util.LogDebug("[MESH] outbound %s: no peer found, passing through", dstIP)
	}
	return false
}

// HandleMeshFrame processes a raw IP packet received from a peer.
func (m *MeshManager) HandleMeshFrame(fromNodeID string, frame []byte) {
	util.LogDebug("[MESH] HandleMeshFrame called from %s: %d bytes", fromNodeID, len(frame))
	if len(frame) < 20 || frame[0]>>4 != 4 {
		util.LogWarn("[MESH] bad packet from %s: %d bytes", fromNodeID, len(frame))
		return
	}

	dstIP := extractDstIP(frame)
	srcIP := net.IP(frame[12:16])
	if dstIP == nil {
		util.LogWarn("[MESH] bad packet from %s: cannot extract dst IP", fromNodeID)
		return
	}
	if isMeshAddress(dstIP) && len(frame) >= 20 && frame[9] == 6 {
		dstPort := uint16(frame[22])<<8 | uint16(frame[23])
		util.LogDebug("[MESH] recv TCP from %s: src=%s dst=%s:%d len=%d", fromNodeID, srcIP, dstIP, dstPort, len(frame))
	}
	util.LogDebug("[MESH] HandleMeshFrame from %s: src=%s dst=%s proto=%d len=%d",
		fromNodeID, srcIP, dstIP, frame[9], len(frame))

	if isMeshAddress(dstIP) {
		util.LogDebug("[MESH] recv frame from %s: dst=%s TTL=%d len=%d", fromNodeID, dstIP, frame[8], len(frame))
		if len(frame) >= 20 && frame[9] == 6 {
			logTCPPacketMesh("[TCP-DEBUG] recv:", frame)
		}
	}

	isVIP := m.isLocalVIP(dstIP)
	util.LogDebug("[MESH] checking VIP: dst=%s isVIP=%v vip=%v", dstIP, isVIP, m.vip)

	// .1 (VIP): NAT reverse + WriteMeshPacket to OS
	// VIP is only for locally-originated connections (via NAT).
	// If NAT reverse fails, drop the packet - it's not a valid response.
	if isVIP {
		util.LogDebug("[MESH] VIP path entered for packet from %s", fromNodeID)
		if m.natTable == nil {
			util.LogWarn("[MESH] VIP packet from %s dropped: natTable is nil", fromNodeID)
			return
		}
		pkt := make([]byte, len(frame))
		copy(pkt, frame)
		
		// Check if this is a DNS response (UDP with src port 53)
		headerLen := int(pkt[0]&0x0f) * 4
		isDNS := pkt[9] == 17 && len(pkt) >= headerLen+4
		if isDNS {
			srcPort := uint16(pkt[headerLen])<<8 | uint16(pkt[headerLen+1])
			isDNS = srcPort == 53
		}
		
		var natPkt []byte
		if isDNS {
			// DNS response: rewrite src to local GIP so the app accepts it
			localGIP := m.getGIP()
			if localGIP != nil {
				natPkt = m.natTable.TranslateInboundWithSrc(pkt, localGIP)
			} else {
				natPkt = m.natTable.TranslateInbound(pkt)
			}
		} else {
			// TCP or other: keep original src IP (Fake-IP for TCP connections)
			natPkt = m.natTable.TranslateInbound(pkt)
		}
		util.LogDebug("[MESH] VIP NAT reverse: isDNS=%v natPkt=%v", isDNS, natPkt != nil)
		if natPkt != nil {
			go func() {
				if m.tun != nil {
					util.LogDebug("[MESH] VIP NAT reverse success: writing packet to TUN src=%s dst=%s len=%d",
						net.IP(natPkt[12:16]), net.IP(natPkt[16:20]), len(natPkt))
					if err := m.tun.WriteMeshPacket(natPkt); err != nil {
						util.LogWarn("[MESH] write VIP packet to TUN failed: %v", err)
					}
				}
			}()
			return
		}
		// NAT reverse failed — likely a DNS response from a remote gateway.
		// The original query was redirected by tryDNSRedirect: src was rewritten
		// from hostIP to VIP, then dst was changed from local GIP to remote GIP.
		// The response comes back: src=remoteGIP, dst=VIP.
		// We need: src=localGIP (what the app queried), dst=hostIP (original app).
		util.LogDebug("[MESH] VIP NAT reverse failed for packet from %s: src=%s dst=%s proto=%d len=%d",
			fromNodeID, net.IP(frame[12:16]), dstIP, frame[9], len(frame))
		pkt2 := make([]byte, len(frame))
		copy(pkt2, frame)
		localGIP := m.getGIP()
		hostIP := m.getHostIP()
		if localGIP != nil && hostIP != nil {
			util.LogDebug("[MESH] VIP fallback: rewriting src=%s→%s dst=%s→%s",
				net.IP(pkt2[12:16]), localGIP, net.IP(pkt2[16:20]), hostIP)
			copy(pkt2[12:16], localGIP.To4())
			copy(pkt2[16:20], hostIP.To4())
			pkt2[10] = 0
			pkt2[11] = 0
			var sum uint32
			hl := int(pkt2[0]&0x0f) * 4
			for i := 0; i < hl-1; i += 2 {
				sum += uint32(pkt2[i])<<8 | uint32(pkt2[i+1])
			}
			for sum>>16 > 0 {
				sum = (sum & 0xffff) + (sum >> 16)
			}
			cksum := ^uint16(sum)
			pkt2[10] = byte(cksum >> 8)
			pkt2[11] = byte(cksum)
			if pkt2[9] == 17 {
				pkt2[hl+6] = 0
				pkt2[hl+7] = 0
			}
			go func() {
				if m.tun != nil {
					util.LogDebug("[MESH] VIP fallback: writing packet to TUN src=%s dst=%s len=%d",
						net.IP(pkt2[12:16]), net.IP(pkt2[16:20]), len(pkt2))
					if err := m.tun.WriteMeshPacket(pkt2); err != nil {
						util.LogWarn("[MESH] write VIP fallback packet to TUN failed: %v", err)
					}
				}
			}()
			return
		}
		util.LogDebug("[MESH] VIP packet from %s dropped: NAT reverse failed and fallback failed", fromNodeID)
		return
	}

	// .2 (hostIP): src rewrite to local GIP + WriteMeshPacket to OS
	// hostIP traffic never went through NAT, so only src rewrite is needed.
	if m.isLocalHostIP(dstIP) {
		pkt := make([]byte, len(frame))
		copy(pkt, frame)
		localGIP := m.getGIP()
		if localGIP != nil {
			if m.natTable != nil {
				srcPkt := m.natTable.RewriteSrcIP(pkt, localGIP)
				if srcPkt != nil {
					pkt = srcPkt
				}
			} else {
				rewritePkt := rewriteSrcIPInPacket(pkt, localGIP)
				if rewritePkt != nil {
					pkt = rewritePkt
				}
			}
		}
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
		if isMeshAddress(dstIP) {
			util.LogDebug("[MESH] recv frame from %s: dst=%s TTL=%d dropped (TTL<=1)", fromNodeID, dstIP, frame[8])
		}
		return
	}

	if isMeshAddress(dstIP) && len(frame) >= 20 && frame[9] == 6 {
		util.LogDebug("[MESH] pre-findPeer: from=%s dst=%s TTL=%d", fromNodeID, dstIP, frame[8])
	}

	peer := m.findPeer(dstIP)
	if peer == nil {
		// No mesh route — we're the gateway for this destination.
		// Inject into local netstack so it goes out via proxy/direct.
		if isMeshAddress(dstIP) {
			proto := "unknown"
			if len(frame) >= 20 {
				switch frame[9] {
				case 6:
					proto = "TCP"
				case 17:
					proto = "UDP"
				}
			}
			util.LogDebug("[MESH] recv frame from %s: dst=%s proto=%s injecting to local netstack", fromNodeID, dstIP, proto)
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

	if isMeshAddress(dstIP) {
		util.LogDebug("[MESH] forwarding from %s: dst=%s to %s", fromNodeID, dstIP, peer.GetNodeID())
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
	allPeers := m.topology.GetAllPeers()

	// Deduplicate by nodeID (multiple connections to same node)
	seen := make(map[string]bool)
	result := make([]MeshPeerInfo, 0)
	for _, p := range allPeers {
		if p.Sender == nil {
			continue
		}
		nodeID := p.NodeID()
		if nodeID == "" || seen[nodeID] {
			continue
		}
		seen[nodeID] = true
		result = append(result, MeshPeerInfo{
			NodeID:   nodeID,
			Direct:   true,
			Subnet:   p.SubnetStr,
			LastSeen: p.LastSeen,
		})
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
				util.LogDebug("[MESH] DNS resolve: %s -> %s", domain, vip)
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

	// Own subnet (Hop=0, NextHop=nil) — occupies the slot first so no peer
	// advertisement (routes or claimedSubnets) can overwrite it.
	_, ownSubnet, _ := net.ParseCIDR(m.subnetStr)
	if ownSubnet != nil {
		best[m.subnetStr] = globalEntry{0, nil, ownSubnet}
	}

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

	util.LogDebug("[MESH] routes recomputed: %d routes", len(routes))
	for _, r := range routes {
		util.LogDebug("[MESH]   %s -> %s", r.Prefix, r.Peer.GetNodeID())
	}

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
	util.LogDebug("[MESH] routes recomputed: %d routes", len(routes))
	for _, r := range routes {
		util.LogDebug("[MESH]   %s -> %s", r.Prefix, r.Peer.GetNodeID())
	}
	// Log domain suffixes for debugging
	util.LogDebug("[MESH] domain trie: own suffixes=%v", domainSuffixes)
	for _, peer := range peers {
		if peer.Sender == nil {
			continue
		}
		var suffixes []string
		for _, entry := range peer.DomainSuffixes {
			suffixes = append(suffixes, entry.Suffix)
		}
		if len(suffixes) > 0 {
			util.LogDebug("[MESH] domain trie: peer %s suffixes=%v", peer.Sender.GetNodeID(), suffixes)
		}
	}
}

// findPeer finds the peer for a destination IP using longest prefix match.
func (m *MeshManager) findPeer(dstIP net.IP) PeerSender {
	m.routesMu.RLock()
	defer m.routesMu.RUnlock()

	for _, route := range m.routes {
		if route.Prefix.Contains(dstIP) {
			if isMeshAddress(dstIP) {
				util.LogDebug("[MESH] findPeer: dst=%s matched route prefix=%s via=%s", dstIP, route.Prefix, route.Peer.GetNodeID())
			}
			return route.Peer
		}
	}
	return nil
}

// findPeers returns all peers that can reach the destination IP.
// Used for load balancing when multiple P2P connections exist to the same node.
func (m *MeshManager) findPeers(dstIP net.IP) []PeerSender {
	m.routesMu.RLock()
	defer m.routesMu.RUnlock()

	// Find the primary peer for this destination
	var primaryPeer PeerSender
	for _, route := range m.routes {
		if route.Prefix.Contains(dstIP) {
			primaryPeer = route.Peer
			break
		}
	}
	if primaryPeer == nil {
		return nil
	}

	// Collect all peers with the same nodeID (multiple P2P connections to same node)
	targetNodeID := primaryPeer.GetNodeID()
	var result []PeerSender
	peers := m.topology.GetAllPeers()
	for _, peer := range peers {
		if peer.Sender != nil && peer.Sender.GetNodeID() == targetNodeID {
			result = append(result, peer.Sender)
		}
	}
	return result
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
	allPeers := m.topology.GetAllPeers()
	if len(allPeers) == 0 {
		return
	}

	m.mu.RLock()
	advertise := make([]string, len(m.advertise))
	copy(advertise, m.advertise)
	domainSuffixes := make([]string, len(m.domainSuffixes))
	copy(domainSuffixes, m.domainSuffixes)
	m.mu.RUnlock()

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
	// Iterate over all peers (including multiple connections to same node)
	for _, peer := range allPeers {
		if peer.Sender == nil {
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
		peer.Sender.SendGossip(data)
	}
}

// rewriteSrcIPInPacket rewrites the source IP in a raw IPv4 packet
// and recomputes the IP header checksum. Used as a fallback when natTable is nil.
func rewriteSrcIPInPacket(pkt []byte, newSrc net.IP) []byte {
	if len(pkt) < 20 || pkt[0]>>4 != 4 {
		return nil
	}
	result := make([]byte, len(pkt))
	copy(result, pkt)
	copy(result[12:16], newSrc.To4())
	result[10] = 0
	result[11] = 0
	var sum uint32
	headerLen := int(result[0]&0x0f) * 4
	for i := 0; i < headerLen-1; i += 2 {
		sum += uint32(result[i])<<8 | uint32(result[i+1])
	}
	for sum>>16 > 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	cksum := ^uint16(sum)
	result[10] = byte(cksum >> 8)
	result[11] = byte(cksum)
	return result
}
