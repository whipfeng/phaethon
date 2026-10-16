package p2p

import (
	"context"
	"fmt"
	"sync"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/core/transport"
	"github.com/multiformats/go-multiaddr"

	"phaethon/config"
	"phaethon/util"
)

const (
	meshProtocol protocol.ID = "/phaethon/mesh/1.0.0"
)

// P2PManager manages multiple libp2p.Host instances (one per proxy).
// Each proxy gets its own host with its own transport, maintaining
// independent links for quality-based routing decisions.
type P2PManager struct {
	// One libp2p.Host per proxy
	hosts map[string]host.Host // proxyName -> libp2p.Host

	// Proxy configurations
	proxies map[string]*config.Proxy // proxyName -> proxy config

	// LocalSeq management
	mu              sync.RWMutex
	localSeqCounter uint32
	localSeqToProxy map[uint32]string // localSeq -> proxyName

	// Link quality monitor
	monitor *LinkQualityMonitor

	// Mesh stream handler
	meshHandler *MeshStreamHandler

	// Local node ID
	localNodeID string

	ctx    context.Context
	cancel context.CancelFunc
}

// NewP2PManager creates a new P2PManager with one libp2p.Host per proxy.
func NewP2PManager(localNodeID string, proxies map[string]*config.Proxy) (*P2PManager, error) {
	ctx, cancel := context.WithCancel(context.Background())

	mgr := &P2PManager{
		hosts:           make(map[string]host.Host),
		proxies:         proxies,
		localSeqToProxy: make(map[uint32]string),
		localNodeID:     localNodeID,
		ctx:             ctx,
		cancel:          cancel,
	}

	// Create mesh stream handler
	mgr.meshHandler = NewMeshStreamHandler(mgr)

	// Create link quality monitor
	mgr.monitor = NewLinkQualityMonitor(mgr, localNodeID)

	// Create one libp2p.Host for each proxy
	for proxyName, proxy := range proxies {
		h, err := mgr.createHostForProxy(proxyName, proxy)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("create host for proxy %s: %w", proxyName, err)
		}
		mgr.hosts[proxyName] = h
		util.LogInfo("P2P", "Created libp2p.Host for proxy: %s (type=%s)", proxyName, proxy.Type)
	}

	util.LogInfo("P2P", "P2PManager initialized with %d hosts", len(mgr.hosts))
	return mgr, nil
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

// AllocateLocalSeq allocates a new localSeq for a proxy.
func (m *P2PManager) AllocateLocalSeq(proxyName string) uint32 {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.localSeqCounter++
	seq := m.localSeqCounter
	m.localSeqToProxy[seq] = proxyName
	return seq
}

// GetProxyForLocalSeq returns the proxyName for a given localSeq.
func (m *P2PManager) GetProxyForLocalSeq(localSeq uint32) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	proxyName, ok := m.localSeqToProxy[localSeq]
	return proxyName, ok
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
			util.LogError("P2P", "Failed to close host for proxy %s: %v", proxyName, err)
		}
	}

	util.LogInfo("P2P", "P2PManager closed")
	return nil
}
