package p2p

import (
	"fmt"
	"io"
	"sync"

	"phaethon/frame"
	"phaethon/util"

	"github.com/libp2p/go-libp2p/core/transport"
)

// MeshStreamHandler handles mesh protocol streams
type MeshStreamHandler struct {
	host *Libp2pHost
	
	// Stream management
	mu      sync.RWMutex
	streams map[string]map[uint32]*MeshStream // proxyName -> localSeq -> stream
}

// MeshStream represents a mesh protocol stream
type MeshStream struct {
	conn       transport.CapableConn
	proxyName  string
	localSeq   uint32
	remotePeer string
	
	// Frame transport for sending/receiving
	frameTransport frame.FrameTransport
	
	// Lifecycle
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// NewMeshStreamHandler creates a new mesh stream handler
func NewMeshStreamHandler(host *Libp2pHost) *MeshStreamHandler {
	return &MeshStreamHandler{
		host:    host,
		streams: make(map[string]map[uint32]*MeshStream),
	}
}

// RegisterStream registers a new mesh stream
func (h *MeshStreamHandler) RegisterStream(proxyName string, localSeq uint32, conn transport.CapableConn, remotePeer string) error {
	// Get frame transport from connection
	var ft frame.FrameTransport
	if tc, ok := conn.(interface{ FrameTransport() frame.FrameTransport }); ok {
		ft = tc.FrameTransport()
	} else {
		return fmt.Errorf("connection does not support frame transport")
	}
	
	stream := &MeshStream{
		conn:           conn,
		proxyName:      proxyName,
		localSeq:       localSeq,
		remotePeer:     remotePeer,
		frameTransport: ft,
		stopCh:         make(chan struct{}),
	}
	
	h.mu.Lock()
	if h.streams[proxyName] == nil {
		h.streams[proxyName] = make(map[uint32]*MeshStream)
	}
	h.streams[proxyName][localSeq] = stream
	h.mu.Unlock()
	
	// Start read loop
	stream.wg.Add(1)
	go stream.readLoop(h)
	
	util.LogInfo("p2p", "Registered mesh stream: proxy=%s localSeq=%d peer=%s", proxyName, localSeq, remotePeer)
	
	return nil
}

// UnregisterStream unregisters a mesh stream
func (h *MeshStreamHandler) UnregisterStream(proxyName string, localSeq uint32) {
	h.mu.Lock()
	stream, ok := h.streams[proxyName][localSeq]
	if ok {
		delete(h.streams[proxyName], localSeq)
		if len(h.streams[proxyName]) == 0 {
			delete(h.streams, proxyName)
		}
	}
	h.mu.Unlock()
	
	if ok && stream != nil {
		close(stream.stopCh)
		stream.wg.Wait()
		util.LogInfo("p2p", "Unregistered mesh stream: proxy=%s localSeq=%d", proxyName, localSeq)
	}
}

// GetStream gets a mesh stream
func (h *MeshStreamHandler) GetStream(proxyName string, localSeq uint32) (*MeshStream, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	
	streams, ok := h.streams[proxyName]
	if !ok {
		return nil, false
	}
	
	stream, ok := streams[localSeq]
	return stream, ok
}

// SendMeshPacket sends a mesh packet through a stream
func (h *MeshStreamHandler) SendMeshPacket(proxyName string, localSeq uint32, packet []byte) error {
	stream, ok := h.GetStream(proxyName, localSeq)
	if !ok {
		return fmt.Errorf("stream not found: proxy=%s localSeq=%d", proxyName, localSeq)
	}
	
	// Send frame
	return stream.frameTransport.Send(frame.FrameMeshPacket, packet, false)
}

// readLoop reads frames from the stream
func (s *MeshStream) readLoop(handler *MeshStreamHandler) {
	defer s.wg.Done()
	defer handler.UnregisterStream(s.proxyName, s.localSeq)
	
	for {
		select {
		case <-s.stopCh:
			return
		default:
			// Read frame
			frameType, payload, err := s.frameTransport.Recv()
			if err != nil {
				if err != io.EOF {
					util.LogError("p2p", "Stream read error: proxy=%s localSeq=%d err=%v", s.proxyName, s.localSeq, err)
				}
				return
			}
			
			// Handle frame
			switch frameType {
			case frame.FrameMeshPacket:
				handler.handleMeshPacket(s, payload)
			default:
				util.LogDebug("p2p", "Unknown frame type: %d", frameType)
			}
		}
	}
}

// handleMeshPacket handles a mesh packet
func (h *MeshStreamHandler) handleMeshPacket(stream *MeshStream, packet []byte) {
	// TODO: Integrate with mesh.MeshManager to handle the packet
	// For now, just log
	util.LogDebug("p2p", "Received mesh packet: proxy=%s localSeq=%d len=%d", stream.proxyName, stream.localSeq, len(packet))
}

// Close closes the mesh stream handler
func (h *MeshStreamHandler) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	
	// Close all streams
	for proxyName, streams := range h.streams {
		for localSeq, stream := range streams {
			close(stream.stopCh)
			stream.conn.Close()
			util.LogDebug("p2p", "Closed mesh stream: proxy=%s localSeq=%d", proxyName, localSeq)
		}
	}
	
	h.streams = make(map[string]map[uint32]*MeshStream)
	
	return nil
}

// LinkQualityMonitor monitors link quality for routing decisions
type LinkQualityMonitor struct {
	host *Libp2pHost
	
	mu       sync.RWMutex
	metrics  map[string]map[uint32]*LinkMetrics // proxyName -> localSeq -> metrics
	stopCh   chan struct{}
}

// LinkMetrics contains link quality metrics
type LinkMetrics struct {
	RTT         float64 // milliseconds
	ErrorRate   float64 // 0.0 - 1.0
	Throughput  int64   // bytes per second
	QualityScore float64 // 0.0 - 100.0
	
	// Statistics
	totalPings  int
	failedPings int
	lastUpdated int64
}

// NewLinkQualityMonitor creates a new link quality monitor
func NewLinkQualityMonitor(host *Libp2pHost) *LinkQualityMonitor {
	return &LinkQualityMonitor{
		host:    host,
		metrics: make(map[string]map[uint32]*LinkMetrics),
		stopCh:  make(chan struct{}),
	}
}

// StartMonitoring starts monitoring a connection
func (m *LinkQualityMonitor) StartMonitoring(proxyName string, localSeq uint32, conn transport.CapableConn) {
	m.mu.Lock()
	if m.metrics[proxyName] == nil {
		m.metrics[proxyName] = make(map[uint32]*LinkMetrics)
	}
	m.metrics[proxyName][localSeq] = &LinkMetrics{
		QualityScore: 100.0, // Start with perfect score
	}
	m.mu.Unlock()
	
	// TODO: Start RTT probing goroutine
	util.LogDebug("p2p", "Started monitoring: proxy=%s localSeq=%d", proxyName, localSeq)
}

// GetLinkMetrics gets link metrics
func (m *LinkQualityMonitor) GetLinkMetrics(proxyName string, localSeq uint32) (*LinkMetrics, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	
	metrics, ok := m.metrics[proxyName][localSeq]
	return metrics, ok
}

// Stop stops the monitor
func (m *LinkQualityMonitor) Stop() {
	close(m.stopCh)
}
