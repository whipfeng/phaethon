package p2p

import (
	"context"
	"fmt"

	"phaethon/config"
	"phaethon/util"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
)

// P2PManager manages P2P connections using real libp2p.Host
type P2PManager struct {
	// Real libp2p host
	host host.Host
	
	// Local node info
	localNodeID string
	
	// Proxy configurations
	proxies map[string]*config.Proxy
	
	// Mesh protocol ID
	meshProtocol protocol.ID
	
	// Context for lifecycle
	ctx    context.Context
	cancel context.CancelFunc
}

// NewP2PManager creates a new P2P manager with real libp2p host
func NewP2PManager(localNodeID string) (*P2PManager, error) {
	ctx, cancel := context.WithCancel(context.Background())
	
	// Create libp2p host with custom options
	h, err := libp2p.New(
		// Disable default transports (we'll add custom ones)
		libp2p.NoTransports,
		
		// Disable listen addresses (we're client-only for proxy connections)
		libp2p.ListenAddrStrings(),
		
		// Enable yamux multiplexing
		libp2p.DefaultMuxers,
		
		// Enable security (noise)
		libp2p.DefaultSecurity,
	)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create libp2p host: %w", err)
	}
	
	pm := &P2PManager{
		host:         h,
		localNodeID:  localNodeID,
		proxies:      make(map[string]*config.Proxy),
		meshProtocol: protocol.ID("/mesh/1.0.0"),
		ctx:          ctx,
		cancel:       cancel,
	}
	
	util.LogInfo("p2p", "P2PManager initialized with libp2p host: nodeID=%s peerID=%s", 
		localNodeID, h.ID())
	
	return pm, nil
}

// AddProxy adds a proxy configuration
func (pm *P2PManager) AddProxy(proxy *config.Proxy) error {
	pm.proxies[proxy.Name] = proxy
	
	// Create and register custom transport for this proxy
	transport, err := NewProxyTransport(proxy)
	if err != nil {
		return fmt.Errorf("failed to create transport for proxy %s: %w", proxy.Name, err)
	}
	
	// Note: In real libp2p, we would add this transport to the host
	// But libp2p doesn't support adding transports dynamically
	// So we need to recreate the host with all transports upfront
	// Or use a different approach
	
	util.LogInfo("p2p", "Added proxy: name=%s type=%s", proxy.Name, proxy.Type)
	
	return nil
}

// Host returns the underlying libp2p host
func (pm *P2PManager) Host() host.Host {
	return pm.host
}

// LocalPeerID returns the local peer ID
func (pm *P2PManager) LocalPeerID() peer.ID {
	return pm.host.ID()
}

// Connect connects to a peer through a specific proxy
func (pm *P2PManager) Connect(proxyName string, peerInfo peer.AddrInfo) error {
	proxy, ok := pm.proxies[proxyName]
	if !ok {
		return fmt.Errorf("proxy not found: %s", proxyName)
	}
	
	// Create custom transport for this proxy
	transport, err := NewProxyTransport(proxy)
	if err != nil {
		return fmt.Errorf("failed to create transport: %w", err)
	}
	
	// Dial through the transport
	conn, err := transport.Dial(pm.ctx, nil, peerInfo.ID)
	if err != nil {
		return fmt.Errorf("failed to dial: %w", err)
	}
	
	// Add connection to host's network
	// Note: This is tricky because libp2p.Host doesn't expose a direct way to add connections
	// We might need to use a different approach
	
	util.LogInfo("p2p", "Connected to peer: proxy=%s peer=%s", proxyName, peerInfo.ID)
	
	// Close the connection for now (placeholder)
	conn.Close()
	
	return nil
}

// Close closes the P2P manager
func (pm *P2PManager) Close() error {
	pm.cancel()
	
	if pm.host != nil {
		pm.host.Close()
	}
	
	util.LogInfo("p2p", "P2PManager closed")
	
	return nil
}
