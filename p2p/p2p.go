package p2p

import (
	"encoding/json"
	"fmt"
	"net"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"phaethon/config"
	"phaethon/dialer"
	"phaethon/reverse"
	"phaethon/util"
)

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
}

// MeshHandler handles mesh packets and gossip from P2P peers.
type MeshHandler interface {
	HandleMeshFrame(fromNodeID string, frame []byte)
	HandleTopologyGossip(fromNodeID string, data []byte)
	RegisterPeer(nodeID, vip string)
	UnregisterPeer(nodeID string)
}

// Peer represents a connected P2P peer.
type Peer struct {
	ID        string         // proxy name used to reach this peer (local only, not serialized)
	NodeID    string         `json:"nodeId"`
	Version   string         `json:"version"`
	Platform  string         `json:"platform"`
	Arch      string         `json:"arch"`
	BuildTag  string         `json:"buildTag,omitempty"`
	Checksum  string         `json:"checksum"`
	Status    string         `json:"status"` // "connecting", "helloed", "updating", "upToDate", "failed"
	LastSeen  time.Time      `json:"lastSeen"`
	Inventory []CacheEntry   `json:"inventory,omitempty"`

	MeshNodeID string `json:"meshNodeId,omitempty"`
	MeshVIP    string `json:"meshVip,omitempty"`

	conn          net.Conn
	stopCh        chan struct{}
	serveFilePath string // file path to serve chunks from (set during update_request handling)

	// Chunk transfer state - prevents deadlock when both sides transfer simultaneously
	transferMu      sync.Mutex
	receivingChunks bool
	chunkCh         chan chunkFrame
}

// chunkFrame represents a frame received during chunk transfer.
type chunkFrame struct {
	frameType byte
	payload   []byte
}

// HelloMsg is exchanged after P2P connection is established.
type HelloMsg struct {
	Cmd       string       `json:"cmd"`
	NodeID    string       `json:"nodeId"`
	Version   string       `json:"version"`
	BuildTag  string       `json:"buildTag,omitempty"`
	Platform  string       `json:"platform"`
	Arch      string       `json:"arch"`
	Checksum  string       `json:"checksum"`
	Inventory []CacheEntry `json:"inventory,omitempty"`
	MeshNodeID string     `json:"meshNodeId,omitempty"`
	MeshVIP    string     `json:"meshVip,omitempty"`
}

// NewP2PManager creates a new P2P manager.
// buildTag is auto-detected at runtime (e.g., "win7" on Windows 7/8).
func NewP2PManager(nodeId, version string, cache *BinaryCache) *P2PManager {
	return &P2PManager{
		peers:    make(map[string]*Peer),
		nodeId:   nodeId,
		version:  version,
		buildTag: DetectBuildTag(),
		platform: runtime.GOOS,
		arch:     runtime.GOARCH,
		cache:    cache,
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
		if peer.MeshNodeID != "" && m.meshHandler != nil {
			m.meshHandler.UnregisterPeer(peer.MeshNodeID)
		}
		delete(m.peers, peer.ID)
		m.mu.Unlock()
		util.LogInfo("[P2P] session ended for %s", peer.ID)
	}()

	go m.sendHeartbeats(peer)
	m.runSession(peer)
}

// StartPeer initiates a P2P connection to a peer through the given proxy.
// It reconnects automatically with exponential backoff if the connection drops.
func (m *P2PManager) StartPeer(proxy *config.Proxy) {
	d := dialer.NewDialer(proxy)
	p2pDialer, ok := d.(dialer.P2PDialer)
	if !ok {
		util.LogDebug("[P2P] proxy %s (%s) does not support P2P", proxy.Name, proxy.Type)
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
		close(peer.stopCh)
		m.mu.Lock()
		if peer.MeshNodeID != "" && m.meshHandler != nil {
			m.meshHandler.UnregisterPeer(peer.MeshNodeID)
		}
		delete(m.peers, peer.ID)
		m.mu.Unlock()
	}()

	backoff := time.Second
	const maxBackoff = 60 * time.Second

	for {
		conn, err := p2pDialer.DialP2P()
		if err != nil {
			util.LogDebug("[P2P] failed to connect to %s via proxy %s: %v", proxy.Server, proxy.Name, err)
			peer.Status = "failed"
			select {
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}

		peer.conn = conn
		peer.Status = "connecting"
		util.LogInfo("[P2P] connected to %s via proxy %s", proxy.Server, proxy.Name)

		go m.sendHeartbeats(peer)
		m.runSession(peer)

		conn.Close()

		util.LogInfo("[P2P] disconnected from %s, reconnecting in %v", peer.ID, backoff)
		peer.Status = "connecting"
		select {
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
			if err := reverse.WriteFrame(peer.conn, reverse.FrameHeartbeat, nil); err != nil {
				return
			}
		}
	}
}

// runSession reads frames and dispatches JSON commands.
func (m *P2PManager) runSession(peer *Peer) {
	// Send hello immediately
	m.sendHello(peer)

	for {
		peer.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		frameType, payload, err := reverse.ReadFrame(peer.conn)
		peer.LastSeen = time.Now()
		if err != nil {
			util.LogInfo("[P2P] read error for %s: %v", peer.ID, err)
			return
		}

		// During chunk transfer, route chunk data and heartbeats to channel but still dispatch commands
		peer.transferMu.Lock()
		if peer.receivingChunks && (frameType == reverse.FrameData || frameType == reverse.FrameHeartbeat) {
			// Check if this is a command (JSON with "cmd" field) or chunk data
			if frameType == reverse.FrameData && len(payload) > 0 {
				var msg map[string]interface{}
				if json.Unmarshal(payload, &msg) == nil {
					if _, hasCmd := msg["cmd"]; hasCmd {
						peer.transferMu.Unlock()
						m.handleCommand(peer, payload)
						continue
					}
				}
			}
			// Not a command, route to chunk channel (includes heartbeats and chunk data)
			ch := peer.chunkCh
			peer.transferMu.Unlock()
			select {
			case ch <- chunkFrame{frameType: frameType, payload: payload}:
			case <-peer.stopCh:
				return
			}
			continue
		}
		peer.transferMu.Unlock()

		switch frameType {
		case reverse.FrameHeartbeat:
			continue
		case reverse.FrameData:
			if len(payload) > 0 {
				m.handleCommand(peer, payload)
			}
		case reverse.FrameMeshPacket:
			if m.meshHandler != nil && len(payload) > 0 {
				util.LogInfo("[P2P] received FrameMeshPacket from %s (%d bytes), meshNodeId=%s", peer.ID, len(payload), peer.MeshNodeID)
				m.meshHandler.HandleMeshFrame(peer.MeshNodeID, payload)
			}
		default:
			util.LogDebug("[P2P] unexpected frame type 0x%02x from %s", frameType, peer.ID)
		}
	}
}

// sendHello sends a hello message to the peer.
func (m *P2PManager) sendHello(peer *Peer) {
	hello := HelloMsg{
		Cmd:      "hello",
		NodeID:   m.nodeId,
		Version:  m.version,
		BuildTag: m.buildTag,
		Platform: m.platform,
		Arch:     m.arch,
	}
	if m.cache != nil {
		hello.Inventory = m.cache.ListInventory()
	}
	if m.meshEnabled {
		hello.MeshNodeID = m.meshNodeID
		hello.MeshVIP = m.meshVIP
	}
	data, _ := json.Marshal(hello)
	reverse.WriteFrame(peer.conn, reverse.FrameData, data)
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
	case "update_request":
		m.handleUpdateRequest(peer, payload)
	case "chunk_req":
		m.handleChunkReq(peer, payload)
	case "manifest":
		// Set up chunk channel before spawning goroutine to prevent race
		peer.transferMu.Lock()
		if peer.receivingChunks {
			peer.transferMu.Unlock()
			util.LogWarn("[P2P] already receiving chunks from %s, ignoring manifest", peer.ID)
			return
		}
		peer.receivingChunks = true
		peer.chunkCh = make(chan chunkFrame, 16)
		peer.transferMu.Unlock()

		go func() {
			defer func() {
				peer.transferMu.Lock()
				peer.receivingChunks = false
				peer.chunkCh = nil
				peer.transferMu.Unlock()
			}()
			m.handleManifest(peer, payload)
		}()
	case "update_ack":
		util.LogInfo("[P2P] update_ack from %s: status=%s", peer.ID, msg["status"])
	case "mesh_gossip":
		if m.meshHandler != nil {
			m.meshHandler.HandleTopologyGossip(peer.MeshNodeID, payload)
		}
	default:
		util.LogDebug("[P2P] unknown command %q from %s", cmd, peer.ID)
	}
}

// handleHello processes a hello message from a peer.
func (m *P2PManager) handleHello(peer *Peer, payload []byte) {
	var hello HelloMsg
	if err := json.Unmarshal(payload, &hello); err != nil {
		util.LogDebug("[P2P] invalid hello from %s: %v", peer.ID, err)
		return
	}

	peer.NodeID = hello.NodeID
	peer.Version = hello.Version
	peer.Platform = hello.Platform
	peer.Arch = hello.Arch
	peer.BuildTag = hello.BuildTag
	peer.Checksum = hello.Checksum
	peer.Inventory = hello.Inventory
	peer.MeshNodeID = hello.MeshNodeID
	peer.MeshVIP = hello.MeshVIP
	peer.Status = "helloed"
	peer.LastSeen = time.Now()

	util.LogInfo("[P2P] hello from %s: node=%s version=%s platform=%s/%s buildTag=%s inventory=%d entries",
		peer.ID, hello.NodeID, hello.Version, hello.Platform, hello.Arch, hello.BuildTag, len(hello.Inventory))

	if hello.MeshNodeID != "" {
		m.mu.Lock()
		for id, p := range m.peers {
			if id != peer.ID && p.MeshNodeID == hello.MeshNodeID {
				util.LogInfo("[P2P] evicting stale peer %s (same meshNodeId=%s, replaced by %s)", id, hello.MeshNodeID, peer.ID)
				p.conn.Close()
				delete(m.peers, id)
			}
		}
		m.mu.Unlock()
		if m.meshHandler != nil {
			m.meshHandler.RegisterPeer(hello.MeshNodeID, hello.MeshVIP)
		}
	}

	// Compare inventories: request entries we don't have or have older versions of
	if m.cache == nil {
		return
	}

	myInventory := m.cache.ListInventory()
	var toRequest []CacheEntry

	for _, peerEntry := range hello.Inventory {
		// Find our entry with matching platform/arch/buildTag
		var myVersion string
		for _, myEntry := range myInventory {
			if myEntry.Platform == peerEntry.Platform && myEntry.Arch == peerEntry.Arch && myEntry.BuildTag == peerEntry.BuildTag {
				myVersion = myEntry.Version
				break
			}
		}

		if myVersion == "" {
			// We don't have this entry at all
			util.LogInfo("[P2P] peer has %s/%s/%s/%s which we don't have, requesting",
				peerEntry.Platform, peerEntry.Arch, peerEntry.BuildTag, peerEntry.Version)
			toRequest = append(toRequest, peerEntry)
		} else if compareVersions(myVersion, peerEntry.Version) < 0 {
			// Peer has a newer version
			util.LogInfo("[P2P] peer has newer %s/%s/%s: %s > %s, requesting",
				peerEntry.Platform, peerEntry.Arch, peerEntry.BuildTag, peerEntry.Version, myVersion)
			toRequest = append(toRequest, peerEntry)
		}
	}

	if len(toRequest) > 0 {
		peer.Status = "updating"
		go func() {
			for _, entry := range toRequest {
				m.requestUpdate(peer, entry)
				// Small delay between requests to avoid overwhelming the peer
				time.Sleep(100 * time.Millisecond)
			}
		}()
	} else {
		util.LogInfo("[P2P] inventories in sync with %s", peer.ID)
		peer.Status = "upToDate"
	}
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

// SendMeshPacket sends a FrameMeshPacket to a peer identified by mesh nodeID.
func (m *P2PManager) SendMeshPacket(peerNodeID string, data []byte) error {
	m.mu.Lock()
	var bestConn net.Conn
	var bestPeerID string
	var bestLastSeen time.Time
	for _, p := range m.peers {
		if p.MeshNodeID == peerNodeID && p.LastSeen.After(bestLastSeen) {
			bestConn = p.conn
			bestPeerID = p.ID
			bestLastSeen = p.LastSeen
		}
	}
	m.mu.Unlock()

	if bestConn == nil {
		util.LogWarn("[P2P] SendMeshPacket: no connection to mesh peer %s", peerNodeID)
		return fmt.Errorf("mesh: no connection to peer %s", peerNodeID)
	}
	util.LogDebug("[P2P] SendMeshPacket: sending %d bytes to %s (peer=%s)", len(data), peerNodeID, bestPeerID)
	return reverse.WriteFrame(bestConn, reverse.FrameMeshPacket, data)
}

// SendMeshPacketByVIP sends a mesh packet to a peer identified by VIP.
func (m *P2PManager) SendMeshPacketByVIP(peerVIP net.IP, data []byte) error {
	m.mu.Lock()
	var bestConn net.Conn
	var bestPeerID string
	var bestLastSeen time.Time
	for _, p := range m.peers {
		if p.MeshVIP != "" {
			peerIP := net.ParseIP(p.MeshVIP)
			if peerIP != nil && peerIP.Equal(peerVIP) && p.LastSeen.After(bestLastSeen) {
				bestConn = p.conn
				bestPeerID = p.ID
				bestLastSeen = p.LastSeen
			}
		}
	}
	m.mu.Unlock()

	if bestConn == nil {
		util.LogWarn("[P2P] SendMeshPacketByVIP: no connection to mesh peer %s", peerVIP)
		return fmt.Errorf("mesh: no connection to peer %s", peerVIP)
	}
	util.LogInfo("[P2P] SendMeshPacketByVIP: sending %d bytes to %s (peer=%s)", len(data), peerVIP, bestPeerID)
	return reverse.WriteFrame(bestConn, reverse.FrameMeshPacket, data)
}

// BroadcastMeshGossip sends a mesh_gossip JSON command to all mesh-enabled peers.
func (m *P2PManager) BroadcastMeshGossip(data []byte) error {
	gossip := make([]byte, len(data))
	copy(gossip, data)

	m.mu.Lock()
	peers := make([]*Peer, 0, len(m.peers))
	for _, p := range m.peers {
		if p.MeshNodeID != "" {
			peers = append(peers, p)
		}
	}
	m.mu.Unlock()

	for _, p := range peers {
		reverse.WriteFrame(p.conn, reverse.FrameData, gossip)
	}
	return nil
}

// ListMeshPeerIDs returns node IDs of all mesh-enabled peers.
func (m *P2PManager) ListMeshPeerIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	var result []string
	for _, p := range m.peers {
		if p.MeshNodeID != "" {
			result = append(result, p.MeshNodeID)
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
			ID:        p.ID,
			NodeID:    p.NodeID,
			Version:   p.Version,
			Platform:  p.Platform,
			Arch:      p.Arch,
			BuildTag:  p.BuildTag,
			Checksum:  p.Checksum,
			Status:    p.Status,
			LastSeen:  p.LastSeen,
			Inventory: p.Inventory,
		})
	}
	return result
}

// GlobalP2PManager is the package-level P2P manager, set from main().
var GlobalP2PManager *P2PManager

// HandleP2PConnection is called from server/p2p_server.go.
func HandleP2PConnection(conn net.Conn, address string) {
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

// compareVersions compares two version strings produced by `git describe --tags --always --dirty`.
// Returns -1 if a < b, 0 if equal, 1 if a > b.
//
// Formats: "v1.2.3", "v1.2.3-5-gabc1234", "v1.2.3-5-gabc1234-dirty", "78e9dfc", "dev"
// Tagged versions are always newer than untagged. More commits after tag = newer.
func compareVersions(a, b string) int {
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
// "v1.2.3" → [1,2,3], 0
// "v1.2.3-5-gabc1234" → [1,2,3], 5
// "v1.2.3-5-gabc1234-dirty" → [1,2,3], 5
// "78e9dfc" → nil, 0
// "dev" → nil, 0
func parseVersion(v string) (tag []int, commits int) {
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimSuffix(v, "-dirty")

	// Split by '-' to separate tag from commit info
	parts := strings.SplitN(v, "-", 3)

	// Try to parse the first part as a version tag (e.g., "1.2.3")
	segments := strings.Split(parts[0], ".")
	allNumeric := true
	for _, s := range segments {
		if _, err := strconv.Atoi(s); err != nil {
			allNumeric = false
			break
		}
	}

	if !allNumeric || len(segments) == 0 {
		return nil, 0
	}

	for _, s := range segments {
		n, _ := strconv.Atoi(s)
		tag = append(tag, n)
	}

	// If there's a commit count (e.g., "5" in "v1.2.3-5-gabc1234")
	if len(parts) >= 2 {
		if n, err := strconv.Atoi(parts[1]); err == nil {
			commits = n
		}
	}

	return tag, commits
}
