package p2p

import (
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"phaethon/config"
	"phaethon/dialer"
	"phaethon/mesh"
	"phaethon/reverse"
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
const P2PProtocolVersion = 6

// P2PManager manages P2P connections to peers.
type P2PManager struct {
	mu        sync.Mutex
	peers     map[string]*Peer
	nodeId    string
	version   string
	buildTag  string
	platform  string
	arch      string
	cache     *BinaryCache

	meshEnabled bool
	meshNodeID  string
	meshVIP     string
	meshHandler MeshHandler

	meshInboundCh    chan meshInboundPacket // queue for async mesh frame processing
	meshInboundStopCh chan struct{}         // stop signal for meshInboundLoop
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
	enqueueWrite(s.peer, reverse.FrameMeshPacket, data)
	return nil
}

func (s *peerSender) SendGossip(data []byte) {
	enqueueWrite(s.peer, reverse.FrameData, data)
}

func (s *peerSender) GetNodeID() string {
	return s.nodeID
}

// writeReq is a frame queued for async write on the peer connection.
type writeReq struct {
	frameType byte
	data      []byte
}

// Peer represents a connected P2P peer.
type Peer struct {
	ID       string    // proxy name used to reach this peer (local only, not serialized)
	NodeID   string    `json:"nodeId"`   // mesh node ID, extracted from hello's ClaimedSubnets[hop=0]
	Status   string    `json:"status"`   // "connecting", "helloed", "upToDate", "failed"
	LastSeen time.Time `json:"lastSeen"`

	conn          net.Conn
	writeCh       chan writeReq
	stopCh        chan struct{}
	stopOnce      sync.Once    // ensures stopCh is closed exactly once
	meshSender    *peerSender  // mesh peer sender, created on hello
}

// NewP2PManager creates a new P2P manager.
// buildTag is auto-detected at runtime (e.g., "win7" on Windows 7/8).
func NewP2PManager(nodeId, version string, cache *BinaryCache) *P2PManager {
	m := &P2PManager{
		peers:    make(map[string]*Peer),
		nodeId:   nodeId,
		version:  version,
		buildTag: DetectBuildTag(),
		platform: runtime.GOOS,
		arch:     runtime.GOARCH,
		cache:    cache,
		meshInboundCh:     make(chan meshInboundPacket, 4096),
		meshInboundStopCh: make(chan struct{}),
	}
	go m.meshInboundLoop()
	return m
}

// peerWriteLoop drains the peer's writeCh and writes frames to the TCP connection.
// Serializes all writes without needing a mutex. Exits on write error or stop.
func (m *P2PManager) peerWriteLoop(peer *Peer) {
	for {
		select {
		case <-peer.stopCh:
			return
		case req, ok := <-peer.writeCh:
			if !ok {
				return
			}
			_ = peer.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if req.frameType == reverse.FrameMeshPacket {
				util.LogDebug("[P2P] writing FrameMeshPacket to %s (nodeID=%s, %d bytes)", peer.ID, peer.NodeID, len(req.data))
			}
			if err := reverse.WriteFrame(peer.conn, req.frameType, req.data); err != nil {
				_ = peer.conn.SetWriteDeadline(time.Time{})
				util.LogWarn("[P2P] write error for %s: %v", peer.ID, err)
				peer.conn.Close()
				return
			}
			_ = peer.conn.SetWriteDeadline(time.Time{})
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

// enqueueWrite queues a frame for async write. Non-blocking: drops if channel is full.
func enqueueWrite(peer *Peer, frameType byte, data []byte) {
	if frameType == reverse.FrameMeshPacket {
		util.LogDebug("[P2P] enqueue FrameMeshPacket to %s (nodeID=%s, %d bytes)", peer.ID, peer.NodeID, len(data))
	}
	select {
	case peer.writeCh <- writeReq{frameType: frameType, data: data}:
	default:
		util.LogDebug("[P2P] write queue full for %s, dropping frame type=0x%02x", peer.ID, frameType)
	}
}

// HandleP2PConn is called from server-side BIND PORT=2 handlers.
// It wraps the connection in the frame protocol and runs the P2P session.
func (m *P2PManager) HandleP2PConn(conn net.Conn, address string) {
	peer := &Peer{
		ID:       conn.RemoteAddr().String(),
		Status:   "connecting",
		LastSeen: time.Now(),
		conn:     conn,
		writeCh:  make(chan writeReq, 1024),
		stopCh:   make(chan struct{}),
	}

	m.mu.Lock()
	m.peers[peer.ID] = peer
	m.mu.Unlock()

	util.LogInfo("[P2P] incoming connection from %s (address=%s)", peer.ID, address)
	defer func() {
		close(peer.stopCh)
		conn.Close()
		m.mu.Lock()
		if peer.meshSender != nil && m.meshHandler != nil {
			m.meshHandler.UnregisterPeer(peer.meshSender)
		}
		delete(m.peers, peer.ID)
		m.mu.Unlock()
		util.LogInfo("[P2P] session ended for %s", peer.ID)
	}()

	go m.peerWriteLoop(peer)
	go m.sendHeartbeats(peer)
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
		if peer.conn != nil {
			peer.conn.Close()
		}
	}
}

// StartPeer initiates a P2P connection to a peer through the given proxy.
// It reconnects automatically with exponential backoff if the connection drops.
// Call StopPeer to permanently disconnect.
func (m *P2PManager) StartPeer(proxy *config.Proxy) {
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

		conn, err := p2pDialer.DialP2P()
		if err != nil {
			util.LogInfo("[P2P] failed to connect to %s via proxy %s: %v", proxy.Server, proxy.Name, err)
			peer.Status = "failed"
			select {
			case <-peer.stopCh:
				return
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}

		peer.conn = conn
		peer.writeCh = make(chan writeReq, 1024)
		peer.Status = "connecting"
		util.LogInfo("[P2P] connected to %s via proxy %s", proxy.Server, proxy.Name)

		// Run session in a closure so we can use defer for cleanup
		func() {
			defer func() {
				conn.Close()
				// Clean up mesh peer registration
				m.mu.Lock()
				if peer.meshSender != nil && m.meshHandler != nil {
					m.meshHandler.UnregisterPeer(peer.meshSender)
				}
				peer.meshSender = nil
				m.mu.Unlock()
			}()

			go m.peerWriteLoop(peer)
			go m.sendHeartbeats(peer)
			m.runSession(peer)
		}()

		util.LogInfo("[P2P] disconnected from %s, reconnecting in %v", peer.ID, backoff)
		peer.Status = "connecting"
		select {
		case <-peer.stopCh:
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// sendHeartbeats sends FrameHeartbeat every 10s on the P2P connection.
func (m *P2PManager) sendHeartbeats(peer *Peer) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-peer.stopCh:
			return
		case <-ticker.C:
			enqueueWrite(peer, reverse.FrameHeartbeat, nil)
		}
	}
}

// runSession reads frames and dispatches JSON commands.
func (m *P2PManager) runSession(peer *Peer) {
	// Send hello immediately
	m.sendHello(peer)

	util.LogInfo("[P2P] runSession started for %s", peer.ID)

	for {
		peer.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		frameType, payload, err := reverse.ReadFrame(peer.conn)
		peer.LastSeen = time.Now()
		if err != nil {
			util.LogInfo("[P2P] read error for %s: %v", peer.ID, err)
			return
		}
		util.LogDebug("[P2P] received frame type=0x%02x len=%d from %s", frameType, len(payload), peer.ID)

		switch frameType {
		case reverse.FrameHeartbeat:
			continue
		case reverse.FrameData:
			if len(payload) > 0 {
				m.handleCommand(peer, payload)
			}
		case reverse.FrameMeshPacket:
			util.LogDebug("[P2P] received FrameMeshPacket from %s (%d bytes), nodeID=%s", peer.ID, len(payload), peer.NodeID)
			if m.meshHandler != nil && len(payload) > 0 {
				util.LogDebug("[P2P] queuing HandleMeshFrame for %s with %d bytes", peer.NodeID, len(payload))
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
	enqueueWrite(peer, reverse.FrameData, data)
}

// handleCommand parses and dispatches a JSON command from a peer.
func (m *P2PManager) handleCommand(peer *Peer, payload []byte) {
	var msg map[string]interface{}
	if err := json.Unmarshal(payload, &msg); err != nil {
		util.LogDebug("[P2P] invalid JSON from %s: %v", peer.ID, err)
		return
	}

	cmd, _ := msg["cmd"].(string)
	switch cmd {
	case "hello":
		m.handleHello(peer, payload)
	case "gossip":
		m.handleGossip(peer, payload)
	default:
		util.LogDebug("[P2P] unknown command %q from %s", cmd, peer.ID)
	}
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
		peer.conn.Close()
		return
	}

	// 2. Extract nodeID from ClaimedSubnets[hop=0]
	nodeID := extractNodeIDFromGossip(info)
	if nodeID == "" {
		util.LogDebug("[P2P] hello from %s has no claimed subnet with hop=0", peer.ID)
		peer.conn.Close()
		return
	}

	peer.NodeID = nodeID
	peer.Status = "helloed"
	peer.LastSeen = time.Now()

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

// BroadcastMeshGossip sends a gossip JSON command to all mesh-enabled peers.
func (m *P2PManager) BroadcastMeshGossip(data []byte) error {
	// Wrap the topology data in a JSON command
	cmd := map[string]interface{}{
		"cmd":     "gossip",
		"payload": json.RawMessage(data),
	}
	gossip, err := json.Marshal(cmd)
	if err != nil {
		return err
	}

	m.mu.Lock()
	peers := make([]*Peer, 0, len(m.peers))
	for _, p := range m.peers {
		if p.meshSender != nil {
			peers = append(peers, p)
		}
	}
	m.mu.Unlock()

	for _, p := range peers {
		enqueueWrite(p, reverse.FrameData, gossip)
	}
	return nil
}

// SendMeshGossipTo sends a gossip JSON command to a specific mesh peer.
func (m *P2PManager) SendMeshGossipTo(peerNodeID string, data []byte) error {
	cmd := map[string]interface{}{
		"cmd":     "gossip",
		"payload": json.RawMessage(data),
	}
	gossip, err := json.Marshal(cmd)
	if err != nil {
		return err
	}

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
	enqueueWrite(target, reverse.FrameData, gossip)
	return nil
}

// SendMeshGossipToAll sends a gossip JSON command to all mesh-enabled peers.
func (m *P2PManager) SendMeshGossipToAll(data []byte) {
	cmd := map[string]interface{}{
		"cmd":     "gossip",
		"payload": json.RawMessage(data),
	}
	gossip, err := json.Marshal(cmd)
	if err != nil {
		return
	}

	m.mu.Lock()
	peers := make([]*Peer, 0, len(m.peers))
	for _, p := range m.peers {
		if p.meshSender != nil {
			peers = append(peers, p)
		}
	}
	m.mu.Unlock()

	for _, p := range peers {
		enqueueWrite(p, reverse.FrameData, gossip)
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
		GlobalP2PManager.HandleP2PConn(conn, address)
	} else {
		util.LogDebug("[P2P] no P2PManager initialized, closing conn from %s", conn.RemoteAddr())
		conn.Close()
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
