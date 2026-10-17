package p2p

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/metrics"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/core/transport"
	"github.com/multiformats/go-multiaddr"
	"gvisor.dev/gvisor/pkg/tcpip"

	"phaethon/config"
	"phaethon/util"
)

const (
	meshProtocol protocol.ID = "/phaethon/mesh/1.0.0"
)

// GlobalP2PManager is the global P2P manager instance.
var GlobalP2PManager *P2PManager

// LookupProxyPeerRoute is a function variable for looking up route info by proxy name.
// It's injected by main.go during initialization.
var LookupProxyPeerRoute func(proxyName string) (tcpip.NICID, *net.IPNet, bool)

// MeshHandler is the interface for mesh network integration.
// TODO: Define proper interface once libp2p integration is complete.
type MeshHandler interface{}

// P2PManager manages multiple libp2p.Host instances (one per proxy).
// Each proxy gets its own host with its own transport, maintaining
// independent links for quality-based routing decisions.
type P2PManager struct {
	// One libp2p.Host per proxy
	hosts map[string]host.Host // proxyName -> libp2p.Host

	// Proxy configurations
	proxies map[string]*config.Proxy // proxyName -> proxy config

	// Peer tracking: maps peer.ID to proxy name and node ID
	peerInfo map[peer.ID]*peerInfo // peer.ID -> info

	// Bandwidth reporter for throughput tracking
	bwReporter *metrics.BandwidthCounter

	// Link quality monitor
	monitor *LinkQualityMonitor

	// Mesh stream handler
	meshHandler *MeshStreamHandler

	// Local node ID
	localNodeID string

	// Mesh info
	meshNodeID string
	meshVIP    string

	// Mesh handler
	meshHandlerCallback MeshHandler

	// Status change callback
	OnStatusChange func()

	ctx    context.Context
	cancel context.CancelFunc
}

// peerInfo tracks information about a connected peer.
type peerInfo struct {
	proxyName string
	nodeID    string
	connID    string
	peerID    peer.ID
}

// NewP2PManager creates a new P2PManager.
// Hosts are created dynamically when StartPeer is called.
func NewP2PManager(localNodeID string) *P2PManager {
	ctx, cancel := context.WithCancel(context.Background())

	mgr := &P2PManager{
		hosts:        make(map[string]host.Host),
		proxies:      make(map[string]*config.Proxy),
		peerInfo:     make(map[peer.ID]*peerInfo),
		bwReporter:   metrics.NewBandwidthCounter(),
		localNodeID:  localNodeID,
		ctx:          ctx,
		cancel:       cancel,
	}

	// Create mesh stream handler
	mgr.meshHandler = NewMeshStreamHandler(mgr)

	// Create link quality monitor
	mgr.monitor = NewLinkQualityMonitor(mgr, localNodeID)

	util.LogModuleInfo("P2P", "P2PManager initialized (localNodeID=%s)", localNodeID)
	return mgr
}

// createHostForProxy creates a libp2p.Host for a specific proxy.
func (m *P2PManager) createHostForProxy(proxyName string, proxy *config.Proxy) (host.Host, error) {
	var t transport.Transport

	switch proxy.Type {
	case config.ProxyTROJAN:
		t = NewTrojanTransport(proxy)
	case config.ProxySOCKS5:
		t = NewSOCKS5Transport(proxy)
	case config.ProxyH_TUNNEL:
		t = NewH_TunnelTransport(proxy)
	default:
		return nil, fmt.Errorf("unsupported proxy type: %s", proxy.Type)
	}

	h, err := libp2p.New(
		libp2p.NoTransports,
		libp2p.Transport(t),
		libp2p.DefaultMuxers, // yamux
		libp2p.DefaultSecurity,
		libp2p.ListenAddrStrings(), // client mode, no listening
		libp2p.BandwidthReporter(m.bwReporter), // Enable bandwidth tracking
	)
	if err != nil {
		return nil, err
	}

	// Register mesh protocol handler
	h.SetStreamHandler(meshProtocol, m.meshHandler.HandleStream)

	return h, nil
}

// Connect connects to a peer through a specific proxy.
func (m *P2PManager) Connect(proxyName string, peerID peer.ID, addr multiaddr.Multiaddr) error {
	h := m.hosts[proxyName]
	if h == nil {
		return fmt.Errorf("no host for proxy: %s", proxyName)
	}

	peerInfo := peer.AddrInfo{ID: peerID, Addrs: []multiaddr.Multiaddr{addr}}
	return h.Connect(m.ctx, peerInfo)
}

// NewStream creates a stream to a peer through a specific proxy.
func (m *P2PManager) NewStream(ctx context.Context, proxyName string, peerID peer.ID) (network.Stream, error) {
	h := m.hosts[proxyName]
	if h == nil {
		return nil, fmt.Errorf("no host for proxy: %s", proxyName)
	}

	return h.NewStream(ctx, peerID, meshProtocol)
}

// GetHost returns the libp2p.Host for a specific proxy.
func (m *P2PManager) GetHost(proxyName string) (host.Host, bool) {
	h, ok := m.hosts[proxyName]
	return h, ok
}

// GetAllHosts returns all libp2p.Host instances.
func (m *P2PManager) GetAllHosts() map[string]host.Host {
	return m.hosts
}

// GetMonitor returns the link quality monitor.
func (m *P2PManager) GetMonitor() *LinkQualityMonitor {
	return m.monitor
}

// GetLocalNodeID returns the local node ID.
func (m *P2PManager) GetLocalNodeID() string {
	return m.localNodeID
}

// Close closes all libp2p.Host instances.
func (m *P2PManager) Close() error {
	m.cancel()

	for proxyName, h := range m.hosts {
		if err := h.Close(); err != nil {
			util.LogModuleError("P2P", "Failed to close host for proxy %s: %v", proxyName, err)
		}
	}

	util.LogModuleInfo("P2P", "P2PManager closed")
	return nil
}

// Stop stops the P2P manager (alias for Close).
func (m *P2PManager) Stop() {
	m.Close()
}

// SetMeshInfo sets the mesh node ID and VIP.
func (m *P2PManager) SetMeshInfo(nodeID, vip string) {
	m.meshNodeID = nodeID
	m.meshVIP = vip
	util.LogModuleInfo("P2P", "Mesh info set: nodeID=%s vip=%s", nodeID, vip)
}

// SetMeshHandler sets the mesh handler callback.
func (m *P2PManager) SetMeshHandler(handler MeshHandler) {
	m.meshHandlerCallback = handler
	util.LogModuleInfo("P2P", "Mesh handler set")
}

// StartPeer starts a P2P peer for the given proxy.
func (m *P2PManager) StartPeer(proxy *config.Proxy) {
	proxyName := proxy.Name

	// Check if already running
	if _, exists := m.hosts[proxyName]; exists {
		util.LogModuleInfo("P2P", "Peer already running for proxy: %s", proxyName)
		return
	}

	// Create host for this proxy
	h, err := m.createHostForProxy(proxyName, proxy)
	if err != nil {
		util.LogModuleError("P2P", "Failed to create host for proxy %s: %v", proxyName, err)
		return
	}

	m.hosts[proxyName] = h
	m.proxies[proxyName] = proxy
	util.LogModuleInfo("P2P", "Started peer for proxy: %s (type=%s)", proxyName, proxy.Type)

	if m.OnStatusChange != nil {
		m.OnStatusChange()
	}
}

// StopPeerByProxyName stops a P2P peer for the given proxy name.
func (m *P2PManager) StopPeerByProxyName(proxyName string) {
	h, exists := m.hosts[proxyName]
	if !exists {
		util.LogModuleInfo("P2P", "Peer not running for proxy: %s", proxyName)
		return
	}

	if err := h.Close(); err != nil {
		util.LogModuleError("P2P", "Failed to close host for proxy %s: %v", proxyName, err)
	}

	delete(m.hosts, proxyName)
	delete(m.proxies, proxyName)
	util.LogModuleInfo("P2P", "Stopped peer for proxy: %s", proxyName)

	if m.OnStatusChange != nil {
		m.OnStatusChange()
	}
}

// GetPeerProxy returns the proxy configuration for a given proxy name.
func (m *P2PManager) GetPeerProxy(proxyName string) *config.Proxy {
	return m.proxies[proxyName]
}

// RestartPeer restarts a P2P peer for the given proxy.
func (m *P2PManager) RestartPeer(proxy *config.Proxy) {
	m.StopPeerByProxyName(proxy.Name)
	m.StartPeer(proxy)
}

// ActivePeerProxyNames returns the names of all active peer proxies.
func (m *P2PManager) ActivePeerProxyNames() []string {
	names := make([]string, 0, len(m.hosts))
	for name := range m.hosts {
		names = append(names, name)
	}
	return names
}

// PeerInfo contains information about a P2P peer.
type PeerInfo struct {
	ProxyName string `json:"proxyName"`
	ProxyType string `json:"proxyType"`
	NodeID    string `json:"nodeId"`
	ConnID    string `json:"connId"`
	Status    string `json:"status"`
}

// GetPeers returns information about all active peers.
func (m *P2PManager) GetPeers() []PeerInfo {
	peers := make([]PeerInfo, 0, len(m.hosts))
	for name, h := range m.hosts {
		proxy := m.proxies[name]
		peerInfo := PeerInfo{
			ProxyName: name,
			ProxyType: proxy.Type,
			NodeID:    m.meshNodeID,
			Status:    "connected",
		}
		// Get ConnID from connections
		conns := h.Network().Conns()
		if len(conns) > 0 {
			peerInfo.ConnID = conns[0].ID()
		}
		peers = append(peers, peerInfo)
	}
	return peers
}

// mesh.P2PTransport interface implementation

// BroadcastMeshGossip broadcasts gossip data to all peers.
func (m *P2PManager) BroadcastMeshGossip(data []byte) error {
	// Send to all connected peers via their streams
	for _, info := range m.peerInfo {
		if err := m.meshHandler.SendPacket(m.ctx, info.proxyName, info.peerID, data); err != nil {
			util.LogModuleError("P2P", "Failed to send gossip to peer %s: %v", info.peerID, err)
		}
	}
	return nil
}

// SendMeshGossipTo sends gossip data to a specific peer.
func (m *P2PManager) SendMeshGossipTo(peerNodeID string, data []byte) error {
	// Find peer by node ID
	for _, info := range m.peerInfo {
		if info.nodeID == peerNodeID {
			return m.meshHandler.SendPacket(m.ctx, info.proxyName, info.peerID, data)
		}
	}
	return fmt.Errorf("peer not found: %s", peerNodeID)
}

// SendMeshGossipToAll sends gossip data to all peers.
func (m *P2PManager) SendMeshGossipToAll(data []byte) {
	m.BroadcastMeshGossip(data)
}

// ListMeshPeerIDs returns a list of connected peer node IDs.
func (m *P2PManager) ListMeshPeerIDs() []string {
	peerIDs := make([]string, 0, len(m.peerInfo))
	for _, info := range m.peerInfo {
		peerIDs = append(peerIDs, info.nodeID)
	}
	return peerIDs
}

// StopPeerByLinkID stops a peer by link ID (ConnID in libp2p).
func (m *P2PManager) StopPeerByLinkID(linkID string) {
	// Find peer by ConnID
	for peerID, info := range m.peerInfo {
		if info.connID == linkID {
			util.LogModuleInfo("P2P", "Stopping peer %s (linkID=%s)", info.nodeID, linkID)
			m.meshHandler.RemoveStream(peerID)
			delete(m.peerInfo, peerID)
			return
		}
	}
	util.LogModuleWarn("P2P", "StopPeerByLinkID: link not found: %s", linkID)
}

// GetLinkQualityStats gets link quality stats for a node.
// Link is identified by nodeID + connID (global unique identifier).
func (m *P2PManager) GetLinkQualityStats(nodeID string) (srtt, rto, jitter time.Duration, lossRate float64, sampleCount int) {
	// Find peer by node ID
	for _, info := range m.peerInfo {
		if info.nodeID == nodeID && info.connID != "" {
			metrics := m.monitor.GetLinkQuality(info.connID)
			if metrics != nil {
				srtt = time.Duration(metrics.RTT) * time.Millisecond
				rto = srtt * 2 // Simple RTO calculation
				jitter = time.Duration(metrics.Jitter) * time.Millisecond
				lossRate = metrics.ErrorRate
				sampleCount = int(metrics.TotalPings)
				
				// Get throughput from libp2p bandwidth reporter
				bwStats := m.bwReporter.GetBandwidthForPeer(info.peerID)
				metrics.Throughput = int64((bwStats.RateIn + bwStats.RateOut) / 2) // Average rate
			}
			return
		}
	}
	return 0, 0, 0, 0.0, 0
}

// GetLinkQualityStatsByProxy gets link quality stats by proxy name.
func (m *P2PManager) GetLinkQualityStatsByProxy(proxyName string) (srtt, rto, jitter time.Duration, lossRate float64, sampleCount int) {
	// Find peer by proxy name and get stats from monitor
	for _, info := range m.peerInfo {
		if info.proxyName == proxyName && info.connID != "" {
			metrics := m.monitor.GetLinkQuality(info.connID)
			if metrics != nil {
				srtt = time.Duration(metrics.RTT) * time.Millisecond
				rto = srtt * 2
				jitter = time.Duration(metrics.Jitter) * time.Millisecond
				lossRate = metrics.ErrorRate
				sampleCount = int(metrics.TotalPings)
				
				// Get throughput from libp2p bandwidth reporter
				bwStats := m.bwReporter.GetBandwidthForPeer(info.peerID)
				metrics.Throughput = int64((bwStats.RateIn + bwStats.RateOut) / 2)
			}
			return
		}
	}
	return 0, 0, 0, 0.0, 0
}

// GetLinkQualityStatsByLinkID gets link quality stats by link ID.
func (m *P2PManager) GetLinkQualityStatsByLinkID(linkID string) (srtt, rto, jitter time.Duration, lossRate float64, sampleCount int) {
	// Get stats directly from monitor using ConnID
	metrics := m.monitor.GetLinkQuality(linkID)
	if metrics != nil {
		srtt = time.Duration(metrics.RTT) * time.Millisecond
		rto = srtt * 2
		jitter = time.Duration(metrics.Jitter) * time.Millisecond
		lossRate = metrics.ErrorRate
		sampleCount = int(metrics.TotalPings)
		
		// Find peer ID for this connID and get throughput
		for peerID, info := range m.peerInfo {
			if info.connID == linkID {
				bwStats := m.bwReporter.GetBandwidthForPeer(peerID)
				metrics.Throughput = int64((bwStats.RateIn + bwStats.RateOut) / 2)
				break
			}
		}
	}
	return
}

// GetChannelDropStats gets channel drop statistics.
// Currently not tracked in libp2p-based implementation.
func (m *P2PManager) GetChannelDropStats() (peerStats map[string]struct {
	DataDrops      uint64
	ControlChDrops uint64
}, meshInboundDrops uint64) {
	// libp2p handles reliability at the transport layer (yamux + TCP)
	// Drop stats are not applicable in the new architecture
	return make(map[string]struct {
		DataDrops      uint64
		ControlChDrops uint64
	}), 0
}

// HandleMeshMessage handles incoming mesh messages.
func (m *P2PManager) HandleMeshMessage(from peer.ID, data []byte) {
	// Forward to mesh handler callback if set
	if m.meshHandlerCallback != nil {
		// TODO: Cast to proper type and call method
		util.LogModuleDebug("P2P", "HandleMeshMessage: from=%s %d bytes", from, len(data))
	}
}
