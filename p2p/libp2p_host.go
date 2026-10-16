package p2p

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"phaethon/config"
	"phaethon/frame"
	"phaethon/util"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/transport"
	"github.com/multiformats/go-multiaddr"
)

// Libp2pHost manages P2P connections using libp2p with custom transports
type Libp2pHost struct {
	mu sync.RWMutex
	
	// Local node ID
	localNodeID string
	
	// Transports indexed by proxyName
	transports map[string]transport.Transport
	
	// Active connections indexed by (proxyName, localSeq)
	outboundConns map[string]map[uint32]transport.CapableConn
	
	// Active connections indexed by localSeq (passive end)
	inboundConns map[uint32]transport.CapableConn
	
	// localSeq -> (proxyName, conn) mapping
	localSeqToProxy map[uint32]string
	
	// Connection counter for generating localSeq
	seqCounter uint32
	
	// Mesh stream handler
	meshHandler *MeshStreamHandler
	
	// Link quality monitor
	monitor *LinkQualityMonitor
	
	// Context for lifecycle management
	ctx    context.Context
	cancel context.CancelFunc
}

// NewLibp2pHost creates a new libp2p host
func NewLibp2pHost(localNodeID string) *Libp2pHost {
	ctx, cancel := context.WithCancel(context.Background())
	
	h := &Libp2pHost{
		localNodeID:     localNodeID,
		transports:      make(map[string]transport.Transport),
		outboundConns:   make(map[string]map[uint32]transport.CapableConn),
		inboundConns:    make(map[uint32]transport.CapableConn),
		localSeqToProxy: make(map[uint32]string),
		ctx:             ctx,
		cancel:          cancel,
	}
	
	// Initialize link quality monitor
	h.monitor = NewLinkQualityMonitor(h)
	
	util.LogInfo("p2p", "Libp2pHost initialized: nodeID=%s", localNodeID)
	
	return h
}

// SetMeshHandler sets the mesh stream handler
func (h *Libp2pHost) SetMeshHandler(handler *MeshStreamHandler) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.meshHandler = handler
}

// GetMonitor returns the link quality monitor
func (h *Libp2pHost) GetMonitor() *LinkQualityMonitor {
	return h.monitor
}

// generateLocalSeq generates a new local sequence number
func (h *Libp2pHost) generateLocalSeq() uint32 {
	return atomic.AddUint32(&h.seqCounter, 1)
}

// RegisterTransport registers a transport for a proxy
func (h *Libp2pHost) RegisterTransport(proxyName string, t transport.Transport) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.transports[proxyName] = t
	util.LogInfo("p2p", "Registered transport: proxy=%s type=%T", proxyName, t)
}

// CreateTransport creates and registers a transport for a proxy config
func (h *Libp2pHost) CreateTransport(proxy *config.Proxy) error {
	localSeq := h.generateLocalSeq()
	
	var t transport.Transport
	switch proxy.Type {
	case config.ProxyTROJAN:
		t = NewTrojanTransport(proxy, localSeq)
	case config.ProxySOCKS5:
		t = NewSOCKS5Transport(proxy, localSeq)
	case config.ProxyH_TUNNEL:
		t = NewH_TunnelTransport(proxy, localSeq)
	default:
		return fmt.Errorf("unsupported proxy type: %s", proxy.Type)
	}
	
	h.RegisterTransport(proxy.Name, t)
	return nil
}

// Connect connects to a peer through a specific proxy
func (h *Libp2pHost) Connect(proxyName string, peerID peer.ID, addr multiaddr.Multiaddr) (transport.CapableConn, error) {
	h.mu.RLock()
	t, ok := h.transports[proxyName]
	h.mu.RUnlock()
	
	if !ok {
		return nil, fmt.Errorf("transport not found: %s", proxyName)
	}
	
	// Dial through the transport
	conn, err := t.Dial(h.ctx, addr, peerID)
	if err != nil {
		return nil, fmt.Errorf("dial failed: %w", err)
	}
	
	// Get local sequence from the connection
	var localSeq uint32
	if tc, ok := conn.(interface{ LocalSequence() uint32 }); ok {
		localSeq = tc.LocalSequence()
	} else {
		localSeq = h.generateLocalSeq()
	}
	
	// Store the connection
	h.mu.Lock()
	if h.outboundConns[proxyName] == nil {
		h.outboundConns[proxyName] = make(map[uint32]transport.CapableConn)
	}
	h.outboundConns[proxyName][localSeq] = conn
	h.localSeqToProxy[localSeq] = proxyName
	h.mu.Unlock()
	
	util.LogInfo("p2p", "Connected to peer: proxy=%s peer=%s localSeq=%d", proxyName, peerID, localSeq)
	
	// Start link quality monitoring
	h.monitor.StartMonitoring(proxyName, localSeq, conn)
	
	return conn, nil
}

// GetOutboundConn gets an outbound connection by proxyName and localSeq
func (h *Libp2pHost) GetOutboundConn(proxyName string, localSeq uint32) (transport.CapableConn, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	
	conns, ok := h.outboundConns[proxyName]
	if !ok {
		return nil, false
	}
	
	conn, ok := conns[localSeq]
	return conn, ok
}

// GetInboundConn gets an inbound connection by localSeq
func (h *Libp2pHost) GetInboundConn(localSeq uint32) (transport.CapableConn, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	
	conn, ok := h.inboundConns[localSeq]
	return conn, ok
}

// GetProxyForLocalSeq gets the proxy name for a local sequence number
func (h *Libp2pHost) GetProxyForLocalSeq(localSeq uint32) (string, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	
	proxyName, ok := h.localSeqToProxy[localSeq]
	return proxyName, ok
}

// GetAllConnsToPeer gets all connections to a specific peer
func (h *Libp2pHost) GetAllConnsToPeer(peerID peer.ID) []transport.CapableConn {
	h.mu.RLock()
	defer h.mu.RUnlock()
	
	var conns []transport.CapableConn
	
	// Check outbound connections
	for _, proxyConns := range h.outboundConns {
		for _, conn := range proxyConns {
			if conn.RemotePeer() == peerID {
				conns = append(conns, conn)
			}
		}
	}
	
	// Check inbound connections
	for _, conn := range h.inboundConns {
		if conn.RemotePeer() == peerID {
			conns = append(conns, conn)
		}
	}
	
	return conns
}

// RemoveConn removes a connection
func (h *Libp2pHost) RemoveConn(proxyName string, localSeq uint32) {
	h.mu.Lock()
	defer h.mu.Unlock()
	
	// Remove from outbound
	if conns, ok := h.outboundConns[proxyName]; ok {
		if conn, ok := conns[localSeq]; ok {
			conn.Close()
			delete(conns, localSeq)
		}
		if len(conns) == 0 {
			delete(h.outboundConns, proxyName)
		}
	}
	
	// Remove from inbound
	if conn, ok := h.inboundConns[localSeq]; ok {
		conn.Close()
		delete(h.inboundConns, localSeq)
	}
	
	// Remove mapping
	delete(h.localSeqToProxy, localSeq)
	
	util.LogInfo("p2p", "Removed connection: proxy=%s localSeq=%d", proxyName, localSeq)
}

// SendFrame sends a frame to a peer through a specific connection
func (h *Libp2pHost) SendFrame(proxyName string, localSeq uint32, frameType byte, payload []byte, isControl bool) error {
	conn, ok := h.GetOutboundConn(proxyName, localSeq)
	if !ok {
		return fmt.Errorf("connection not found: proxy=%s localSeq=%d", proxyName, localSeq)
	}
	
	// Get frame transport from connection
	if tc, ok := conn.(interface{ FrameTransport() frame.FrameTransport }); ok {
		ft := tc.FrameTransport()
		return ft.Send(frameType, payload, isControl)
	}
	
	return fmt.Errorf("connection does not support frame transport")
}

// Close closes the host and all connections
func (h *Libp2pHost) Close() error {
	h.cancel()
	
	h.mu.Lock()
	defer h.mu.Unlock()
	
	// Close all outbound connections
	for proxyName, conns := range h.outboundConns {
		for localSeq, conn := range conns {
			conn.Close()
			util.LogDebug("p2p", "Closed outbound connection: proxy=%s localSeq=%d", proxyName, localSeq)
		}
	}
	
	// Close all inbound connections
	for localSeq, conn := range h.inboundConns {
		conn.Close()
		util.LogDebug("p2p", "Closed inbound connection: localSeq=%d", localSeq)
	}
	
	// Stop monitor
	if h.monitor != nil {
		h.monitor.Stop()
	}
	
	util.LogInfo("p2p", "Libp2pHost closed")
	
	return nil
}

// LocalNodeID returns the local node ID
func (h *Libp2pHost) LocalNodeID() string {
	return h.localNodeID
}
