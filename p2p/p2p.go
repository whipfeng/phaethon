package p2p

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"phaethon/frame"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"phaethon/config"
	"phaethon/dialer"
	"phaethon/mesh"
	"phaethon/util"
)

// commitPattern matches the git describe commit info: -<digits>-g<hash>
var commitPattern = regexp.MustCompile(`-(\d+)-g[0-9a-f]+$`)

// P2PProtocolVersion is the current P2P protocol version.
// Bump when making incompatible changes to the P2P frame protocol or hello semantics.
// Version 3: Gossip protocol redesigned - GossipInfo removed NodeID/Subnet fields,
// Routes/DomainSuffixes now reference nodeID, ClaimedSubnets added Neighbors field,
// mutual neighbor validation added to prevent stale claim wandering.
// Version 4: DNS resolution optimized - .phn domains treated as static routes (not gossiped),
// ResolveDomainSubnet returns (subnet, needsFail) for proper SERVFAIL handling.
// Version 5: two-trie DNS routing (16281a8) changed cross-node .phn answer semantics
// (static trie answers even when the owning node is down; no remote fallback); mixing
// pre/post versions silently wedges remote .phn resolution after node restarts.
// Version 6: hello/gossip unified - both use GossipInfo structure, hello carries topology
// for immediate route establishment, sender nodeID extracted from ClaimedSubnets[hop=0],
// removed unused HelloMsg fields (NodeID/Version/Platform/Arch/BuildTag/Checksum/MeshNodeID/MeshVIP),
// renamed "mesh_gossip" to "gossip".
// Version 7: control frame reliable delivery with seq/ack, removed heartbeat/probe/probe_reply,
// gossip serves as keepalive, link quality measured from ack timing.
const P2PProtocolVersion = 7

// P2PManager manages P2P connections to peers.
type P2PManager struct {
	mu       sync.Mutex
	peers    map[string]*Peer
	nodeId   string
	version  string
	buildTag string
	platform string
	arch     string
	cache    *BinaryCache

	meshEnabled bool
	meshNodeID  string
	meshVIP     string
	meshHandler MeshHandler

	meshInboundCh     chan meshInboundPacket // queue for async mesh frame processing
	meshInboundStopCh chan struct{}          // stop signal for meshInboundLoop

	OnStatusChange func() // callback when peer status changes
}

type meshInboundPacket struct {
	fromNodeID string
	frame      []byte
}

// MeshHandler handles mesh packets and gossip from P2P peers.
type MeshHandler interface {
	HandleMeshFrame(fromNodeID string, frame []byte)
	HandleTopologyGossip(sender mesh.PeerSender, data []byte)
	RegisterPeer(sender mesh.PeerSender)
	UnregisterPeer(sender mesh.PeerSender)
	UnregisterPeerByNodeID(nodeID string)
	BuildGossipInfo() *mesh.GossipInfo
}

// peerSender wraps a P2P peer connection to implement mesh.PeerSender.
type peerSender struct {
	peer   *Peer
	nodeID string // mesh node ID, extracted from hello's ClaimedSubnets[hop=0]
}

func (s *peerSender) Send(data []byte) error {
	return enqueueWrite(s.peer, frame.FrameMeshPacket, data, false) // data frame, fire-and-forget
}

func (s *peerSender) SendGossip(data []byte) {
	enqueueWrite(s.peer, frame.FrameGossip, data, true) // control frame, reliable delivery
}

func (s *peerSender) GetNodeID() string {
	return s.nodeID
}

// writeReq is a frame queued for async write on the peer connection.
type writeReq struct {
	frameType byte
	data      []byte
	isControl bool
}

// Peer represents a connected P2P peer.
type Peer struct {
	ID       string    `json:"name"`   // proxy name used to reach this peer
	NodeID   string    `json:"nodeId"` // mesh node ID, extracted from hello's ClaimedSubnets[hop=0]
	Status   string    `json:"status"` // "connecting", "helloed", "upToDate", "failed"
	LastSeen time.Time `json:"lastSeen"`

	transport  frame.FrameTransport
	writeCh    chan writeReq
	controlCh  chan writeReq // hello/gossip priority queue
	stopCh     chan struct{}
	stopOnce   sync.Once   // ensures stopCh is closed exactly once
	meshSender *peerSender // mesh peer sender, created on hello
	proxy      *config.Proxy // proxy config this peer was started with

	// Reliable transmission state (v7)
	sendState *sendState // tracks outgoing control frames
	recvState *recvState // tracks incoming control frames
}

// NewP2PManager creates a new P2P manager.
// buildTag is auto-detected at runtime (e.g., "win7" on Windows 7/8).
func NewP2PManager(nodeId, version string, cache *BinaryCache) *P2PManager {
	m := &P2PManager{
		peers:             make(map[string]*Peer),
		nodeId:            nodeId,
		version:           version,
		buildTag:          DetectBuildTag(),
		platform:          runtime.GOOS,
		arch:              runtime.GOARCH,
		cache:             cache,
		meshInboundCh:     make(chan meshInboundPacket, 16384),
		meshInboundStopCh: make(chan struct{}),
	}
	go m.meshInboundLoop()
	return m
}

// GetPeerStatus returns the P2P connection status for all peers.
// Returns a map of proxy name -> status info.
func (m *P2PManager) GetPeerStatus() map[string]map[string]interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	result := make(map[string]map[string]interface{})
	for name, peer := range m.peers {
		result[name] = map[string]interface{}{
			"status":   peer.Status,
			"nodeId":   peer.NodeID,
			"lastSeen": peer.LastSeen,
		}
	}
	return result
}

// peerWriteLoop drains the peer's queues and writes frames via the peer
// transport. Control frames (hello/gossip) take priority over mesh
// data so they are not delayed behind bulk transfers. Exits on write error or stop.
// For control frames, encodes seq/ack into the payload (8 bytes: seq(4) + ack(4)).
func (m *P2PManager) peerWriteLoop(peer *Peer) {
	writeFrame := func(req writeReq, isControl bool) bool {
		payload := req.data
		if isControl {
			// Encode seq/ack into control frame payload
			seq := peer.sendState.allocSeq()
			ack := peer.recvState.getLastSeq()
			// Prepend 8 bytes: seq(4) + ack(4)
			encoded := make([]byte, 8+len(payload))
			binary.BigEndian.PutUint32(encoded[0:4], seq)
			binary.BigEndian.PutUint32(encoded[4:8], ack)
			copy(encoded[8:], payload)
			payload = encoded
			// Record as sent for retransmission
			peer.sendState.markSent(seq, req.frameType, req.data)
		}
		if err := peer.transport.Send(req.frameType, payload, isControl); err != nil {
			util.LogWarn("[P2P] write error for %s: %v", peer.ID, err)
			peer.transport.Close()
			return false
		}
		return true
	}

	for {
		// Fast path: control frames first, non-blocking.
		select {
		case req := <-peer.controlCh:
			if !writeFrame(req, true) {
				return
			}
			continue
		default:
		}
		select {
		case <-peer.stopCh:
			return
		case req := <-peer.controlCh:
			if !writeFrame(req, true) {
				return
			}
		case req, ok := <-peer.writeCh:
			if !ok {
				return
			}
			if !writeFrame(req, false) {
				return
			}
		}
	}
}

// meshInboundLoop consumes mesh frames from meshInboundCh and calls HandleMeshFrame.
// Decouples mesh processing from P2P read loop to prevent blocking.
func (m *P2PManager) meshInboundLoop() {
	for {
		select {
		case <-m.meshInboundStopCh:
			return
		case pkt := <-m.meshInboundCh:
			if m.meshHandler != nil {
				m.meshHandler.HandleMeshFrame(pkt.fromNodeID, pkt.frame)
			}
		}
	}
}

// ErrPeerStopped is returned when sending to a peer that has been stopped.
var ErrPeerStopped = fmt.Errorf("peer stopped")

// ErrQueueFull is returned when the peer's write queue is full.
var ErrQueueFull = fmt.Errorf("write queue full")

// enqueueWrite queues a frame for async write. Non-blocking: drops if the
// queue is full (drop-tail; overlay TCP retransmission recovers mesh data).
// Control frames (isControl=true) use a small priority queue so they
// never queue behind bulk mesh data.
// Returns error: nil if enqueued, ErrPeerStopped if peer stopped, ErrQueueFull if queue full.
func enqueueWrite(peer *Peer, frameType byte, data []byte, isControl bool) error {
	// Check if peer is stopped
	select {
	case <-peer.stopCh:
		return ErrPeerStopped
	default:
	}

	ch := peer.writeCh
	if isControl {
		ch = peer.controlCh
	}
	select {
	case ch <- writeReq{frameType: frameType, data: data, isControl: isControl}:
		return nil
	default:
		util.LogDebug("[P2P] write queue full for %s, dropping frame type=0x%02x", peer.ID, frameType)
		return ErrQueueFull
	}
}

// HandleP2PTransport runs a P2P session over an established frame transport
// (server side). The transport is owned by the manager and closed when the
// session ends.
func (m *P2PManager) HandleP2PTransport(t frame.FrameTransport, address string) {
	peer := &Peer{
		ID:        address,
		Status:    "connecting",
		LastSeen:  time.Now(),
		transport: t,
		writeCh:   make(chan writeReq, 16384),
		controlCh: make(chan writeReq, 512),
		stopCh:    make(chan struct{}),
		sendState: newSendState(),
		recvState: newRecvState(),
	}

	m.mu.Lock()
	m.peers[peer.ID] = peer
	m.mu.Unlock()

	defer func() {
		close(peer.stopCh)
		t.Close()
		m.mu.Lock()
		if peer.meshSender != nil && m.meshHandler != nil {
			m.meshHandler.UnregisterPeer(peer.meshSender)
		}
		delete(m.peers, peer.ID)
		m.mu.Unlock()
		util.LogInfo("[P2P] session ended for %s", peer.ID)
	}()

	go m.peerWriteLoop(peer)
	m.runSession(peer)
}

// StopPeer disconnects a P2P peer permanently.
// The peer will not reconnect after being stopped.
func (m *P2PManager) StopPeer(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if peer, ok := m.peers[id]; ok {
		util.LogInfo("[P2P] stopping peer %s", id)
		peer.stopOnce.Do(func() {
			close(peer.stopCh)
		})
		if peer.transport != nil {
			peer.transport.Close()
		}
	}
}

// StopPeerByNodeID disconnects a P2P peer by its mesh node ID.
// Used when connectivity checks indicate the peer is unreachable.
func (m *P2PManager) StopPeerByNodeID(nodeID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, peer := range m.peers {
		if peer.NodeID == nodeID {
			util.LogInfo("[P2P] stopping peer %s (nodeID=%s) due to connectivity failure", id, nodeID)
			peer.stopOnce.Do(func() {
				close(peer.stopCh)
			})
			if peer.transport != nil {
				peer.transport.Close()
			}
			return
		}
	}
}

// GetPeerProxy returns the proxy config that a peer was started with.
// Returns nil if the peer does not exist.
func (m *P2PManager) GetPeerProxy(id string) *config.Proxy {
	m.mu.Lock()
	defer m.mu.Unlock()
	if peer, ok := m.peers[id]; ok {
		return peer.proxy
	}
	return nil
}

// RestartPeer stops an existing peer and starts a new one with updated config.
// This is used when proxy config (password, server, etc.) changes.
func (m *P2PManager) RestartPeer(proxy *config.Proxy) {
	util.LogInfo("[P2P] restarting peer for proxy %s due to config change", proxy.Name)
	m.StopPeer(proxy.Name)
	go m.StartPeer(proxy)
}

// StartPeer initiates a P2P connection to a peer through the given proxy.
// It reconnects automatically with exponential backoff if the connection drops.
// Call StopPeer to permanently disconnect.
func (m *P2PManager) StartPeer(proxy *config.Proxy) {
	util.LogInfo("[P2P] StartPeer called for proxy %s (type=%s, p2p=%v)", proxy.Name, proxy.Type, proxy.IsP2P())
	d := dialer.NewDialer(proxy)
	p2pDialer, ok := d.(dialer.P2PDialer)
	if !ok {
		util.LogInfo("[P2P] proxy %s (%s) does not support P2P", proxy.Name, proxy.Type)
		return
	}

	peer := &Peer{
		ID:       proxy.Name,
		Status:   "connecting",
		LastSeen: time.Now(),
		stopCh:   make(chan struct{}),
		proxy:    proxy,
	}

	m.mu.Lock()
	m.peers[peer.ID] = peer
	m.mu.Unlock()

	defer func() {
		peer.stopOnce.Do(func() {
			close(peer.stopCh)
		})
		m.mu.Lock()
		if peer.meshSender != nil && m.meshHandler != nil {
			m.meshHandler.UnregisterPeer(peer.meshSender)
		}
		delete(m.peers, peer.ID)
		m.mu.Unlock()
	}()

	backoff := time.Second
	const maxBackoff = 60 * time.Second

	for {
		// Check if peer has been stopped before attempting to connect
		select {
		case <-peer.stopCh:
			util.LogInfo("[P2P] peer %s stopped, exiting reconnect loop", peer.ID)
			return
		default:
		}

		transport, err := p2pDialer.DialP2P()
		if err != nil {
			util.LogInfo("[P2P] failed to connect to %s via proxy %s: %v", proxy.Server, proxy.Name, err)
			peer.Status = "failed"
			if m.OnStatusChange != nil {
				go m.OnStatusChange()
			}
			// Add jitter: delay = backoff * (0.5 + rand[0,1)) = backoff * [0.5, 1.5)
			jitteredBackoff := time.Duration(float64(backoff) * (0.5 + rand.Float64()))
			select {
			case <-peer.stopCh:
				return
			case <-time.After(jitteredBackoff):
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}

		peer.transport = transport
		peer.writeCh = make(chan writeReq, 16384)
		peer.controlCh = make(chan writeReq, 512)
		peer.sendState = newSendState()
		peer.recvState = newRecvState()
		peer.Status = "connecting"
		if m.OnStatusChange != nil {
			go m.OnStatusChange()
		}
		util.LogInfo("[P2P] connected to %s via proxy %s", proxy.Server, proxy.Name)

		// Run session in a closure so we can use defer for cleanup
		func() {
			defer func() {
				transport.Close()
				// Clean up mesh peer registration
				m.mu.Lock()
				if peer.meshSender != nil && m.meshHandler != nil {
					m.meshHandler.UnregisterPeer(peer.meshSender)
				}
				peer.meshSender = nil
				m.mu.Unlock()
			}()

			go m.peerWriteLoop(peer)
			m.runSession(peer)
		}()

		util.LogInfo("[P2P] disconnected from %s, reconnecting in %v", peer.ID, backoff)
		peer.Status = "connecting"
		if m.OnStatusChange != nil {
			go m.OnStatusChange()
		}
		// Add jitter: delay = backoff * (0.5 + rand[0,1)) = backoff * [0.5, 1.5)
		jitteredBackoff := time.Duration(float64(backoff) * (0.5 + rand.Float64()))
		select {
		case <-peer.stopCh:
			return
		case <-time.After(jitteredBackoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// runSession reads frames and dispatches them.
func (m *P2PManager) runSession(peer *Peer) {
	// Send hello immediately
	m.sendHello(peer)

	util.LogInfo("[P2P] runSession started for %s", peer.ID)

	// Heartbeat timeout monitor: close connection if no data received for 30s
	heartbeatTimeout := 30 * time.Second
	checkInterval := 10 * time.Second
	timeoutDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(checkInterval)
		defer ticker.Stop()
		for {
			select {
			case <-timeoutDone:
				return
			case <-peer.stopCh:
				return
			case <-ticker.C:
				if time.Since(peer.LastSeen) > heartbeatTimeout {
					util.LogWarn("[P2P] heartbeat timeout for %s (last seen %v ago), closing connection", peer.ID, time.Since(peer.LastSeen))
					peer.transport.Close()
					return
				}
			}
		}
	}()
	defer close(timeoutDone)

	// Retransmission monitor: periodically check for timed-out control frames
	retransmitDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-retransmitDone:
				return
			case <-peer.stopCh:
				return
			case <-ticker.C:
				m.checkRetransmissions(peer)
			}
		}
	}()
	defer close(retransmitDone)

	for {
		frameType, payload, err := peer.transport.Recv()
		peer.LastSeen = time.Now()
		if err != nil {
			util.LogInfo("[P2P] read error for %s: %v", peer.ID, err)
			return
		}
		util.LogDebug("[P2P] received frame type=0x%02x len=%d from %s", frameType, len(payload), peer.ID)

		// Control frames have seq/ack encoded in the first 8 bytes
		isControl := frameType == frame.FrameHello || frameType == frame.FrameGossip
		if isControl {
			if len(payload) < 8 {
				util.LogWarn("[P2P] control frame too short from %s: len=%d", peer.ID, len(payload))
				continue
			}
			seq := binary.BigEndian.Uint32(payload[0:4])
			ack := binary.BigEndian.Uint32(payload[4:8])
			payload = payload[8:] // strip seq/ack header

			// Process ack (removes acknowledged frames from pending)
			peer.sendState.processAck(ack)

			// Check for duplicate
			if !peer.recvState.checkAndRecord(seq) {
				util.LogDebug("[P2P] duplicate control frame from %s: seq=%d", peer.ID, seq)
				continue
			}
		}

		switch frameType {
		case frame.FrameHello:
			if len(payload) > 0 {
				m.handleHello(peer, payload)
			}
		case frame.FrameGossip:
			if len(payload) > 0 {
				m.handleGossip(peer, payload)
			}
		case frame.FrameMeshPacket:
			if m.meshHandler != nil && len(payload) > 0 {
				frameCopy := make([]byte, len(payload))
				copy(frameCopy, payload)
				select {
				case m.meshInboundCh <- meshInboundPacket{fromNodeID: peer.NodeID, frame: frameCopy}:
					// Queued for async processing
				default:
					util.LogDebug("[P2P] meshInboundCh full, dropping frame from %s", peer.NodeID)
				}
			} else {
				util.LogWarn("[P2P] FrameMeshPacket dropped: meshHandler=%v payloadLen=%d", m.meshHandler != nil, len(payload))
			}
		default:
			util.LogInfo("[P2P] unexpected frame type 0x%02x from %s (%d bytes)", frameType, peer.ID, len(payload))
		}
	}
}

// checkRetransmissions checks for timed-out control frames and retransmits them.
// Uses exponential backoff: 1s → 2s → 4s → 8s, max 5 retries.
func (m *P2PManager) checkRetransmissions(peer *Peer) {
	rto := peer.sendState.rttSampler.getRTO()
	pending := peer.sendState.getRetransmissions(rto)

	for _, pf := range pending {
		if pf.retries >= 5 {
			// Give up after 5 retries
			util.LogWarn("[P2P] giving up on control frame to %s: seq=%d retries=%d", peer.ID, pf.seq, pf.retries)
			peer.sendState.removeFrame(pf.seq)
			continue
		}

		util.LogDebug("[P2P] retransmitting control frame to %s: seq=%d retries=%d", peer.ID, pf.seq, pf.retries)

		// Re-encode with current ack
		ack := peer.recvState.getLastSeq()
		encoded := make([]byte, 8+len(pf.payload))
		binary.BigEndian.PutUint32(encoded[0:4], pf.seq)
		binary.BigEndian.PutUint32(encoded[4:8], ack)
		copy(encoded[8:], pf.payload)

		if err := peer.transport.Send(pf.frameType, encoded, true); err != nil {
			util.LogWarn("[P2P] retransmit write error for %s: %v", peer.ID, err)
			peer.transport.Close()
			return
		}
		peer.sendState.markRetransmitted(pf.seq)
	}
}

// sendHello sends a hello message to the peer, carrying gossip topology for immediate route establishment.
func (m *P2PManager) sendHello(peer *Peer) {
	info := mesh.GossipInfo{
		Cmd:             "hello",
		ProtocolVersion: P2PProtocolVersion,
	}

	// Build gossip content from mesh handler
	if m.meshHandler != nil {
		gossipInfo := m.meshHandler.BuildGossipInfo()
		info.DomainSuffixes = gossipInfo.DomainSuffixes
		info.Routes = gossipInfo.Routes
		info.ClaimedSubnets = gossipInfo.ClaimedSubnets
	}

	data, _ := json.Marshal(info)
	enqueueWrite(peer, frame.FrameHello, data, true) // control frame, reliable delivery
}

// handleHello processes a hello message from a peer.
func (m *P2PManager) handleHello(peer *Peer, payload []byte) {
	var info mesh.GossipInfo
	if err := json.Unmarshal(payload, &info); err != nil {
		util.LogDebug("[P2P] invalid hello from %s: %v", peer.ID, err)
		return
	}

	// 1. Version check
	if info.ProtocolVersion != P2PProtocolVersion {
		util.LogWarn("[P2P] protocol version mismatch from %s: peer=%d local=%d, disconnecting",
			peer.ID, info.ProtocolVersion, P2PProtocolVersion)
		peer.transport.Close()
		return
	}

	// 2. Extract nodeID from ClaimedSubnets[hop=0]
	nodeID := extractNodeIDFromGossip(info)
	if nodeID == "" {
		util.LogDebug("[P2P] hello from %s has no claimed subnet with hop=0", peer.ID)
		peer.transport.Close()
		return
	}

	peer.NodeID = nodeID
	peer.Status = "helloed"
	peer.LastSeen = time.Now()

	if m.OnStatusChange != nil {
		go m.OnStatusChange()
	}

	util.LogInfo("[P2P] hello from %s: nodeID=%s", peer.ID, nodeID)

	// 3. Clean up old state + re-register
	if m.meshHandler != nil {
		m.meshHandler.UnregisterPeerByNodeID(nodeID)
		ps := &peerSender{peer: peer, nodeID: nodeID}
		peer.meshSender = ps
		m.meshHandler.RegisterPeer(ps)
	}

	// 4. Process topology (shared logic)
	m.processGossipInfo(peer, info)

	peer.Status = "upToDate"
	if m.OnStatusChange != nil {
		go m.OnStatusChange()
	}
}

// handleGossip processes a gossip message from a peer.
func (m *P2PManager) handleGossip(peer *Peer, payload []byte) {
	var info mesh.GossipInfo
	if err := json.Unmarshal(payload, &info); err != nil {
		util.LogDebug("[P2P] invalid gossip from %s: %v", peer.ID, err)
		return
	}

	// Process topology (shared logic)
	m.processGossipInfo(peer, info)
}

// processGossipInfo is the unified topology processing logic for both hello and gossip.
func (m *P2PManager) processGossipInfo(peer *Peer, info mesh.GossipInfo) {
	if m.meshHandler != nil && peer.meshSender != nil {
		data, _ := json.Marshal(info)
		m.meshHandler.HandleTopologyGossip(peer.meshSender, data)
	}
}

// extractNodeIDFromGossip extracts the sender's nodeID from ClaimedSubnets with hop=0.
func extractNodeIDFromGossip(info mesh.GossipInfo) string {
	for _, cs := range info.ClaimedSubnets {
		if cs.Hop == 0 {
			return cs.NodeID
		}
	}
	return ""
}

// SetMeshInfo configures mesh networking parameters.
func (m *P2PManager) SetMeshInfo(nodeID, vip string) {
	m.meshEnabled = true
	m.meshNodeID = nodeID
	m.meshVIP = vip
}

// SetMeshHandler sets the mesh handler for processing mesh frames and gossip.
func (m *P2PManager) SetMeshHandler(h MeshHandler) {
	m.meshHandler = h
}

// BroadcastMeshGossip sends gossip to all mesh-enabled peers.
// In v7, data is sent directly as FrameGossip (no JSON command wrapper).
func (m *P2PManager) BroadcastMeshGossip(data []byte) error {
	m.mu.Lock()
	peers := make([]*Peer, 0, len(m.peers))
	for _, p := range m.peers {
		if p.meshSender != nil {
			peers = append(peers, p)
		}
	}
	m.mu.Unlock()

	for _, p := range peers {
		enqueueWrite(p, frame.FrameGossip, data, true) // control frame, reliable delivery
	}
	return nil
}

// SendMeshGossipTo sends gossip to a specific mesh peer.
// In v7, data is sent directly as FrameGossip (no JSON command wrapper).
func (m *P2PManager) SendMeshGossipTo(peerNodeID string, data []byte) error {
	m.mu.Lock()
	var target *Peer
	for _, p := range m.peers {
		if p.meshSender != nil && p.meshSender.nodeID == peerNodeID {
			target = p
			break
		}
	}
	m.mu.Unlock()

	if target == nil {
		return nil
	}
	enqueueWrite(target, frame.FrameGossip, data, true) // control frame, reliable delivery
	return nil
}

// SendMeshGossipToAll sends gossip to all mesh-enabled peers.
// In v7, data is sent directly as FrameGossip (no JSON command wrapper).
func (m *P2PManager) SendMeshGossipToAll(data []byte) {
	m.mu.Lock()
	peers := make([]*Peer, 0, len(m.peers))
	for _, p := range m.peers {
		if p.meshSender != nil {
			peers = append(peers, p)
		}
	}
	m.mu.Unlock()

	for _, p := range peers {
		enqueueWrite(p, frame.FrameGossip, data, true) // control frame, reliable delivery
	}
}

// ResendHelloToAll resends hello to all connected peers (used after subnet conflict resolution).
func (m *P2PManager) ResendHelloToAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, peer := range m.peers {
		m.sendHello(peer)
	}
	util.LogDebug("[P2P] resent hello to all peers (%d)", len(m.peers))
}

// ListMeshPeerIDs returns node IDs of all mesh-enabled peers.
func (m *P2PManager) ListMeshPeerIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	var result []string
	for _, p := range m.peers {
		if p.meshSender != nil {
			result = append(result, p.meshSender.nodeID)
		}
	}
	return result
}

// GetPeers returns a snapshot of all current peers for the admin API.
func (m *P2PManager) GetPeers() []Peer {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := make([]Peer, 0, len(m.peers))
	for _, p := range m.peers {
		result = append(result, Peer{
			ID:       p.ID,
			NodeID:   p.NodeID,
			Status:   p.Status,
			LastSeen: p.LastSeen,
		})
	}
	return result
}

// GetLinkQualityStats returns ACK-based link quality stats for a peer by nodeID.
// Returns srtt, rto, lossRate. Returns zeros if peer not found.
func (m *P2PManager) GetLinkQualityStats(nodeID string) (srtt, rto time.Duration, lossRate float64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, p := range m.peers {
		if p.NodeID == nodeID && p.sendState != nil {
			return p.sendState.getStats()
		}
	}
	return 0, 0, 0
}

// GlobalP2PManager is the package-level P2P manager, set from main().
var GlobalP2PManager *P2PManager

// HandleP2PConnection is called from server/p2p_server.go.
func HandleP2PConnection(conn net.Conn, address string) {
	// Unwrap ReverseFramedConn if present: P2P runs its own frame protocol
	// (ReadFrame/WriteFrame) and must operate on the raw TCP connection.
	// Without unwrapping, WriteFrame would double-wrap inside FrameData,
	// making the data unreadable by the peer.
	if framed, ok := conn.(interface{ Unwrap() net.Conn }); ok {
		conn = framed.Unwrap()
	}
	if GlobalP2PManager != nil {
		util.LogInfo("[P2P] incoming connection from %s (address=%s)", conn.RemoteAddr(), address)
		GlobalP2PManager.HandleP2PTransport(frame.NewStreamTransport(conn), conn.RemoteAddr().String())
	} else {
		util.LogDebug("[P2P] no P2PManager initialized, closing conn from %s", conn.RemoteAddr())
		conn.Close()
	}
}

// HandleP2PTransport is called from server-side mesh channel handlers.
func HandleP2PTransport(t frame.FrameTransport, address string) {
	if GlobalP2PManager != nil {
		util.LogInfo("[P2P] incoming mesh transport from %s", address)
		GlobalP2PManager.HandleP2PTransport(t, address)
	} else {
		util.LogDebug("[P2P] no P2PManager initialized, dropping transport from %s", address)
		t.Close()
	}
}

// VersionString returns a human-readable version string for the manager.
func (m *P2PManager) VersionString() string {
	return fmt.Sprintf("%s/%s/%s", m.version, m.platform, m.arch)
}

// BuildInfo returns the running binary's version, platform, arch and buildTag
// (recorded at manager construction from main.Version).
func (m *P2PManager) BuildInfo() (version, platform, arch, buildTag string) {
	return m.version, m.platform, m.arch, m.buildTag
}

// runningVersion returns the running binary's version for self-update logging.
func runningVersion() string {
	if GlobalP2PManager != nil {
		return GlobalP2PManager.version
	}
	return "unknown"
}

// CacheInventory returns the p2p-cache entries with file sizes and mod times.
// Used by the admin console to show the node's binary version history.
func (m *P2PManager) CacheInventory() []CacheEntryInfo {
	if m.cache == nil {
		return nil
	}
	return m.cache.Inventory()
}

// CacheFilePath returns the file path of a cache entry (for admin publish).
func (m *P2PManager) CacheFilePath(platform, arch, buildTag, version string) (string, error) {
	if m.cache == nil {
		return "", fmt.Errorf("p2p cache not initialized")
	}
	return m.cache.FilePath(platform, arch, buildTag, version)
}

// CompareVersions compares two version strings produced by `git describe --tags --always --dirty`.
// Returns -1 if a < b, 0 if equal, 1 if a > b.
//
// Formats: "v1.2.3", "v1.2.3-5-gabc1234", "v1.2.3-5-gabc1234-dirty", "78e9dfc", "dev"
// Tagged versions are always newer than untagged. More commits after tag = newer.
func CompareVersions(a, b string) int {
	if a == b {
		return 0
	}

	// "dev" is the lowest possible version
	if a == "dev" {
		return -1
	}
	if b == "dev" {
		return 1
	}

	aTag, aCommits := parseVersion(a)
	bTag, bCommits := parseVersion(b)

	// Both have tags: compare tag segments, then commit count
	if aTag != nil && bTag != nil {
		for i := 0; i < len(aTag) || i < len(bTag); i++ {
			av, bv := 0, 0
			if i < len(aTag) {
				av = aTag[i]
			}
			if i < len(bTag) {
				bv = bTag[i]
			}
			if av < bv {
				return -1
			}
			if av > bv {
				return 1
			}
		}
		// Tags are equal, compare commit count
		if aCommits < bCommits {
			return -1
		}
		if aCommits > bCommits {
			return 1
		}
		return 0
	}

	// Tagged is always newer than untagged
	if aTag != nil && bTag == nil {
		return 1
	}
	if aTag == nil && bTag != nil {
		return -1
	}

	// Both untagged (hashes or "dev"): fall back to string comparison
	if a < b {
		return -1
	}
	return 1
}

// parseVersion extracts numeric tag segments and commit count from a version string.
// Supports Semver build metadata format: v1.2.3+build[-commits-ghash][-dirty]
//
// Examples:
//
//	"v1.2.3" → [1,2,3], 0
//	"v1.2.3+mesh" → [1,2,3], 0
//	"v1.2.3+mesh-5-gabc1234" → [1,2,3], 5
//	"v1.2.3-5-gabc1234" → [1,2,3], 5
//	"v1.2.3+mesh-5-gabc1234-dirty" → [1,2,3], 5
//	"78e9dfc" → nil, 0
//	"dev" → nil, 0
func parseVersion(v string) (tag []int, commits int) {
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimSuffix(v, "-dirty")

	// Try to extract commit count from the end: -<digits>-g<hash>
	// Pattern: something-N-ghash where N is the commit count
	if match := commitPattern.FindStringSubmatch(v); match != nil {
		if n, err := strconv.Atoi(match[1]); err == nil {
			commits = n
		}
		// Remove the -N-ghash part to get the tag
		v = v[:len(v)-len(match[0])]
	}

	// Split by '+' to separate version from build metadata
	// e.g., "1.2.3+mesh" → "1.2.3" (we ignore "+mesh" for comparison)
	if idx := strings.Index(v, "+"); idx != -1 {
		v = v[:idx]
	}

	// Parse version segments (e.g., "1.2.3" → [1, 2, 3])
	segments := strings.Split(v, ".")
	if len(segments) == 0 {
		return nil, 0
	}

	allNumeric := true
	for _, s := range segments {
		if _, err := strconv.Atoi(s); err != nil {
			allNumeric = false
			break
		}
	}

	if !allNumeric {
		return nil, 0
	}

	for _, s := range segments {
		n, _ := strconv.Atoi(s)
		tag = append(tag, n)
	}

	return tag, commits
}
