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
	"sync/atomic"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
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
// Version 8: link-state routing with quality-aware path selection.
// GossipClaimedSubnet.Neighbors extended from []string to []GossipNeighbor,
// carrying per-link SRTT and loss rate metrics for Dijkstra-based routing.
// Version 9: GossipLink changed from linkId to localSeq+remoteSeq fields,
// link identification now based on seq pair matching instead of combined linkId.
const P2PProtocolVersion = 9

// meshInboundWorkers is the number of goroutines that consume from meshInboundCh
// and process incoming mesh frames (inject into gVisor netstack).
//
// Rationale: With 1 worker, we observed channelDrops=161 (queue overflow) under
// moderate load. 4 workers provide 4x throughput improvement, eliminating drops
// while keeping resource overhead low (~32KB total stack space). gVisor's
// DeliverNetworkPacket is thread-safe (uses atomic counters and RWMutex for
// shared state), so concurrent injection from multiple workers is safe.
//
// Tuning: Monitor channelDrops in P2P-STATS logs. If drops reappear under peak
// load, increase this value. If CPU usage is high with low throughput, consider
// reducing it. Typical range: 2-8 workers.
const meshInboundWorkers = 4

// LookupProxyPeerRoute is injected by main to resolve a proxy name to its
// peer's LinkNIC ID and subnet. Used by v2 route callbacks to bypass the
// RouteSelector for proxy-selected mesh links.
var LookupProxyPeerRoute func(proxyName string) (linkNICID tcpip.NICID, peerSubnet *net.IPNet, ok bool)

func init() {
	LookupProxyPeerRoute = func(proxyName string) (tcpip.NICID, *net.IPNet, bool) {
		mgr := GlobalP2PManager
		if mgr == nil {
			util.LogModuleError("p2p", "[V2-LOOKUP] GlobalP2PManager is nil for proxy=%s", proxyName)
			return 0, nil, false
		}
		mgr.mu.Lock()
		defer mgr.mu.Unlock()
		if peer, ok := mgr.proxyPeers[proxyName]; ok {
			// Use thread-safe GetLinkNIC() (fix race condition #2)
			linkNIC := peer.GetLinkNIC()
			if linkNIC == nil {
				util.LogModuleError("p2p", "[V2-LOOKUP] peer found but linkNIC is nil for proxy=%s peer.ID=%s status=%s", proxyName, peer.ID, peer.GetStatus())
				return 0, nil, false
			}
			nicID := linkNIC.NICID()
			subnet := linkNIC.PeerSubnet()
			util.LogModuleInfo("p2p", "[V2-LOOKUP] proxy=%s peer.ID=%s linkNICID=%d subnet=%s status=%s", proxyName, peer.ID, nicID, subnet, peer.GetStatus())
			return nicID, subnet, true
		}
		util.LogModuleError("p2p", "[V2-LOOKUP] no peer found for proxy=%s", proxyName)
		return 0, nil, false
	}
}

// linkStats holds persistent statistics for a link (survives P2P reconnections).
type linkStats struct {
	sendState      *sendState
	dataDrops      uint64 // total data queue drops (dns + ack + data[256])
	controlChDrops uint64
}

// P2PManager manages P2P connections to peers.
type P2PManager struct {
	mu         sync.Mutex
	peers      map[string]*Peer    // keyed by peer.ID (localSeq-based; active + passive)
	proxyPeers map[string]*Peer    // keyed by proxy.Name (active peers only, for dedup)
	nodeId     string
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
	stopOnce          sync.Once
	sessionsWG        sync.WaitGroup
	stopped           bool

	linkSeqCounter uint64 // atomic counter for link sequence numbers

	// Channel overflow statistics (atomic counters)
	meshInboundDrops uint64 // Frames dropped due to meshInboundCh full

	OnStatusChange func() // callback when peer status changes

	// Debounce mechanism for OnStatusChange (fix race condition #5)
	statusChangeTimer *time.Timer
	statusChangeMu    sync.Mutex
}

type meshInboundPacket struct {
	fromNodeID string
	linkNIC    *mesh.LinkNIC // 链路直接持有的 LinkNIC 指针
	frame      []byte
}

// MeshHandler handles mesh packets and gossip from P2P peers.
type MeshHandler interface {
	HandleMeshFrame(fromNodeID string, linkNIC *mesh.LinkNIC, frame []byte)
	HandleTopologyGossip(sender mesh.PeerSender, data []byte)
	RegisterPeer(sender mesh.PeerSender)
	UnregisterPeer(sender mesh.PeerSender)
	UnregisterPeerByNodeID(nodeID string)
	BuildGossipInfo() *mesh.GossipInfo
	// LinkNIC 生命周期管理（设计 §5.6）
	CreateLinkNIC(sender mesh.PeerSender, nodeID, subnet string) *mesh.LinkNIC
	DeleteLinkNIC(linkNIC *mesh.LinkNIC)
}

// peerSender wraps a P2P peer connection to implement mesh.PeerSender.
type peerSender struct {
	peer   *Peer
	nodeID string // mesh node ID, extracted from hello's ClaimedSubnets[hop=0]
}

func (s *peerSender) Send(data []byte) error {
	util.LogModuleDebug("p2p", "[P2P-SENDER-DIAG] Send called nodeID=%s peer.ID=%s dataLen=%d", s.nodeID, s.peer.ID, len(data))
	return enqueueWrite(s.peer, frame.FrameMeshPacket, data, false) // data frame, fire-and-forget
}

func (s *peerSender) SendGossip(data []byte) {
	enqueueWrite(s.peer, frame.FrameGossip, data, true) // control frame, reliable delivery
}

func (s *peerSender) GetNodeID() string {
	return s.nodeID
}

func (s *peerSender) GetProxyName() string {
	return s.peer.ProxyName
}

// GetLinkID returns the negotiated link ID (from hello negotiation).
// Deprecated: Use GetLocalSeq/GetRemoteSeq for gossip advertisement.
func (s *peerSender) GetLinkID() string {
	return s.peer.LinkID
}

// GetLocalSeq returns the local sequence number for this link.
func (s *peerSender) GetLocalSeq() uint16 {
	return s.peer.Seq
}

// GetRemoteSeq returns the remote sequence number for this link.
func (s *peerSender) GetRemoteSeq() uint16 {
	return s.peer.RemoteSeq
}

// GetFriendlyName returns the friendly name (for display only).
// Passive peers (no proxy) return empty string - they should not advertise friendly names.
func (s *peerSender) GetFriendlyName() string {
	if s.peer.proxy == nil {
		return "" // Passive peer - no friendly name
	}
	name := s.peer.FriendlyName
	if name == "" {
		name = s.peer.ProxyName // proxy name fallback
	}
	return name
}

// HasProxy returns true if this peer was created with a proxy config (active/outbound connection).
// Passive (inbound) peers have no proxy and should not advertise links.
func (s *peerSender) HasProxy() bool {
	return s.peer.proxy != nil
}

// GetLinkNIC returns the LinkNIC directly held by this link (design §5.6).
func (s *peerSender) GetLinkNIC() *mesh.LinkNIC {
	return s.peer.GetLinkNIC()
}

// writeReq is a frame queued for async write on the peer connection.
type writeReq struct {
	frameType      byte
	data           []byte
	isControl      bool
	encodedControl bool
}

// DRRScheduler implements Deficit Round Robin fair scheduling for multiple data queues.
// v2: 256 queues (was 16) + Jenkins hash + batch dequeue.
type DRRScheduler struct {
	queues   [256]chan writeReq
	deficits [256]int
	quantum  int // bytes per queue per round (default 1500 = MTU)
	round    int // next queue to check
	salt     uint32 // random salt for Jenkins hash
	notifyCh chan struct{} // signaled when a packet is enqueued
}

// newDRRScheduler creates a new DRR scheduler with the given queues.
func newDRRScheduler(queues [256]chan writeReq) *DRRScheduler {
	return &DRRScheduler{
		queues:   queues,
		quantum:  1500, // MTU
		round:    0,
		salt:     rand.Uint32(),
		notifyCh: make(chan struct{}, 1), // buffered channel for notification
	}
}

// Notify signals that a packet has been enqueued.
// Called by enqueueWriteReq when a packet is added to a data queue.
func (s *DRRScheduler) Notify() {
	select {
	case s.notifyCh <- struct{}{}:
	default:
		// Already notified, don't block
	}
}

// Next returns the next packet to send using DRR fair scheduling.
// Blocks until a packet is available or stopCh is closed.
func (s *DRRScheduler) Next(stopCh <-chan struct{}) *writeReq {
	batch := s.NextBatch(1, stopCh)
	if len(batch) > 0 {
		return &batch[0]
	}
	return nil
}

// NextBatch returns up to maxBatch packets using DRR fair scheduling.
// v2: batch dequeue to reduce channel overhead.
// Does ONE round through all 256 queues, collecting packets fairly.
// If no packets are found, blocks until a packet arrives or stopCh is closed.
// Next round starts from where this round stopped (fair across rounds).
func (s *DRRScheduler) NextBatch(maxBatch int, stopCh <-chan struct{}) []writeReq {
	batch := make([]writeReq, 0, maxBatch)
	
	// Do ONE round through all 256 queues (or until batch is full)
	roundsChecked := 0
	for i := 0; i < 256 && len(batch) < maxBatch; i++ {
		idx := (s.round + i) % 256
		roundsChecked = i + 1

		// Replenish deficit if needed
		if s.deficits[idx] <= 0 {
			s.deficits[idx] += s.quantum
		}

		// Non-blocking try to dequeue
		select {
		case req := <-s.queues[idx]:
			s.deficits[idx] -= len(req.data)
			batch = append(batch, req)
		default:
		}
	}
	
	// Update round for next time: start from where we stopped
	s.round = (s.round + roundsChecked) % 256
	
	// If we got at least one packet, return immediately
	if len(batch) > 0 {
		return batch
	}
	
	// No packets found, block until a packet arrives or stopped
	for {
		select {
		case <-stopCh:
			return batch
		case <-s.notifyCh:
			// A packet arrived, do another round
			roundsChecked = 0
			for i := 0; i < 256 && len(batch) < maxBatch; i++ {
				idx := (s.round + i) % 256
				roundsChecked = i + 1

				if s.deficits[idx] <= 0 {
					s.deficits[idx] += s.quantum
				}

				select {
				case req := <-s.queues[idx]:
					s.deficits[idx] -= len(req.data)
					batch = append(batch, req)
				default:
				}
			}
			s.round = (s.round + roundsChecked) % 256
			
			// If we got packets, return; otherwise loop and wait again
			if len(batch) > 0 {
				return batch
			}
			// Stale notification or packet consumed elsewhere, wait again
		}
	}
}

// Peer represents a connected P2P peer.
type Peer struct {
	ID        string `json:"id"`        // unique connection ID (localSeq-based)
	ProxyName string `json:"proxyName"` // proxy config name (active peers only, empty for passive)
	NodeID    string `json:"nodeId"`    // mesh node ID, extracted from hello's ClaimedSubnets[hop=0]
	Status    string `json:"status"`    // current peer status: connecting, helloed, upToDate, failed
	LastError string `json:"lastError,omitempty"` // last error message (empty if no error)
	LastSeen  time.Time `json:"lastSeen"`

	// LinkID negotiation (hello phase)
	Seq          uint16 // 本端生成的序列号（本地递增）
	RemoteSeq    uint16 // 对端声明的序列号（hello 协商获得）
	LinkID       string // 协商后的链路 ID（seq1-seq2 排序拼接，仅内部使用）
	FriendlyName string // 友好名称（代理名，仅展示）

	// Atomic fields for concurrent access (fix race condition #2 and #4)
	statusAtomic   atomic.Value // string: "connecting", "helloed", "upToDate", "failed"
	linkNICPtr     atomic.Pointer[mesh.LinkNIC]
	meshSenderPtr  atomic.Pointer[peerSender]
	lastSeenNano   atomic.Int64 // UnixNano timestamp of last seen (thread-safe)

	transport        frame.FrameTransport // protected by mutex (atomic.Pointer cannot wrap interface)
	dnsQueue         chan writeReq // DNS packets (UDP port 53, highest priority)
	ackQueue         chan writeReq // TCP ACK packets (high priority)
	dataQueues       [256]chan writeReq // per-flow data queues (DRR fair scheduling, v2: 256 queues)
	drrOutput        chan writeReq // DRR scheduler output channel
	controlCh        chan writeReq // hello/gossip priority queue
	stopCh           chan struct{}
	stopOnce         sync.Once // ensures stopCh is closed exactly once
	sessionDone      chan struct{} // closed when session fully ends (including LinkNIC cleanup)
	meshSender       *peerSender   // DEPRECATED: use meshSenderPtr
	proxy            *config.Proxy // proxy config this peer was started with

	// Reliable transmission state (v7)
	sendState *sendState // tracks outgoing control frames
	recvState *recvState // tracks incoming control frames

	// LinkNIC: 链路直接持有自己的 LinkNIC 指针（设计 §5.4）
	// DEPRECATED: use linkNICPtr
	linkNIC *mesh.LinkNIC

	// DRR scheduler for data queues
	drrScheduler *DRRScheduler

	// Per-peer random number generator (avoid global rand lock contention)
	rng   *rand.Rand
	rngMu sync.Mutex

	// Pre-allocated buffer for pure ACK (avoid 4-byte allocation per ACK)
	ackBuf [4]byte

	// Channel overflow statistics (atomic counters)
	dnsQueueDrops       uint64 // Frames dropped due to dnsQueue full
	ackQueueDrops       uint64 // Frames dropped due to ackQueue full
	dataQueueDrops      [256]uint64 // Frames dropped due to dataQueues[i] full (v2: 256 queues)
	controlChDrops      uint64 // Frames dropped due to controlCh full
}

// Status getter/setter (thread-safe)
func (p *Peer) GetStatus() string {
	if v := p.statusAtomic.Load(); v != nil {
		return v.(string)
	}
	return ""
}

func (p *Peer) SetStatus(status string) {
	p.statusAtomic.Store(status)
}

// LinkNIC getter/setter (thread-safe, fix race condition #2)
func (p *Peer) GetLinkNIC() *mesh.LinkNIC {
	return p.linkNICPtr.Load()
}

func (p *Peer) SetLinkNIC(nic *mesh.LinkNIC) {
	p.linkNICPtr.Store(nic)
}

// MeshSender getter/setter (thread-safe)
func (p *Peer) GetMeshSender() *peerSender {
	return p.meshSenderPtr.Load()
}

func (p *Peer) SetMeshSender(s *peerSender) {
	p.meshSenderPtr.Store(s)
}

// ProxyNameSafe returns the proxy name, or empty string if proxy is nil (passive/inbound peer).
func (p *Peer) ProxyNameSafe() string {
	if p.proxy == nil {
		return ""
	}
	return p.proxy.Name
}

// NewP2PManager creates a new P2P manager.
// buildTag is auto-detected at runtime (e.g., "win7" on Windows 7/8).
func NewP2PManager(nodeId, version string, cache *BinaryCache) *P2PManager {
	frame.SetMeshPacketTraceHook(func(stage string, payload []byte) {
		mesh.TraceForwarding(stage, payload, "")
	})
	m := &P2PManager{
		peers:             make(map[string]*Peer),
		proxyPeers:        make(map[string]*Peer),
		nodeId:            nodeId,
		version:           version,
		buildTag:          DetectBuildTag(),
		platform:          runtime.GOOS,
		arch:              runtime.GOARCH,
		cache:             cache,
		meshInboundCh:     make(chan meshInboundPacket, 4096),
		meshInboundStopCh: make(chan struct{}),
	}
	// Start multiple worker goroutines to consume from meshInboundCh in parallel.
	// This increases throughput and reduces queue overflow (channelDrops).
	for i := 0; i < meshInboundWorkers; i++ {
		go m.meshInboundLoop()
	}
	return m
}

func (m *P2PManager) beginSession() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return false
	}
	m.sessionsWG.Add(1)
	return true
}

func stopPeer(peer *Peer) {
	peer.stopOnce.Do(func() {
		close(peer.stopCh)
	})
	if peer.transport != nil {
		peer.transport.Close()
	}
}

// Stop closes every active P2P session and waits for reconnect and session loops to exit.
func (m *P2PManager) Stop() {
	m.stopOnce.Do(func() {
		m.mu.Lock()
		m.stopped = true
		peers := make([]*Peer, 0, len(m.peers))
		for _, peer := range m.peers {
			peers = append(peers, peer)
		}
		m.mu.Unlock()

		for _, peer := range peers {
			stopPeer(peer)
		}
		m.sessionsWG.Wait()
		close(m.meshInboundStopCh)
	})
}

// GetPeerStatus returns the P2P connection status for all peers.
// Returns a map of proxy name -> status info (active peers only).
func (m *P2PManager) GetPeerStatus() map[string]map[string]interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := make(map[string]map[string]interface{}, len(m.proxyPeers))
	for name, peer := range m.proxyPeers {
		lastSeenNano := peer.lastSeenNano.Load()
		result[name] = map[string]interface{}{
			"status":   peer.GetStatus(),
			"nodeId":   peer.NodeID,
			"lastSeen": time.Unix(0, lastSeenNano),
		}
	}
	return result
}

// notifyStatusChange debounces OnStatusChange calls.
// Multiple rapid status changes within 100ms are coalesced into a single callback.
func (m *P2PManager) notifyStatusChange() {
	if m.OnStatusChange == nil {
		return
	}

	m.statusChangeMu.Lock()
	defer m.statusChangeMu.Unlock()

	// Cancel any pending timer
	if m.statusChangeTimer != nil {
		m.statusChangeTimer.Stop()
	}

	// Schedule new callback after 100ms debounce window
	m.statusChangeTimer = time.AfterFunc(100*time.Millisecond, func() {
		m.OnStatusChange()
	})
}

// peerWriteLoop drains the peer's queues and writes frames via the peer
// transport. Control frames (hello/gossip) take priority over mesh
// data so they are not delayed behind bulk transfers. Exits on write error or stop.
// For control frames, encodes seq/ack into the payload (8 bytes: seq(4) + ack(4)).
// Data frames are coalesced into batches for efficient transport.
func (m *P2PManager) peerWriteLoop(peer *Peer) {
	const maxBatchFrames = 64 // Maximum frames per batch

	// encodeControlFrame encodes seq/ack into control frame payload.
	encodeControlFrame := func(req writeReq) []byte {
		payload := req.data
		if req.frameType == frame.FrameAck {
			// Pure ACK format: {seq(4)=0, ack(4)}
			ack := binary.BigEndian.Uint32(payload[0:4])
			encoded := make([]byte, 8)
			binary.BigEndian.PutUint32(encoded[0:4], 0)
			binary.BigEndian.PutUint32(encoded[4:8], ack)
			return encoded
		} else if !req.encodedControl {
			// Encode seq/ack into control frame payload
			seq := peer.sendState.allocSeq()
			ack := peer.recvState.getLastSeq()
			encoded := make([]byte, 8+len(payload))
			binary.BigEndian.PutUint32(encoded[0:4], seq)
			binary.BigEndian.PutUint32(encoded[4:8], ack)
			copy(encoded[8:], payload)
			peer.sendState.markSent(seq, req.frameType, req.data)
			return encoded
		}
		return payload
	}

	// writeSingleFrame writes a single control frame.
	writeSingleFrame := func(req writeReq, isControl bool) bool {
		payload := req.data
		if isControl {
			payload = encodeControlFrame(req)
		}

		if req.frameType == frame.FrameMeshPacket {
			mesh.TraceForwarding("p2p_write_start", req.data, "peer=%s", peer.ID)
			// Diagnostic: log IPIP packets (proto=4) sent via writeSingleFrame
			if len(req.data) >= 20 && req.data[0]>>4 == 4 && req.data[9] == 4 {
				util.LogModuleDebug("p2p", "[P2P-IPIP-SEND-SINGLE] peer=%s len=%d", peer.ID, len(req.data))
			}
		}
		if req.frameType == frame.FrameHello {
			util.LogModuleDebug("p2p", "[P2P-DIAG] sending FrameHello to %s, payload=%d bytes isControl=%v", peer.ID, len(payload), isControl)
		}
		if err := peer.transport.Send(req.frameType, payload, isControl); err != nil {
			if req.frameType == frame.FrameMeshPacket {
				mesh.TraceForwarding("p2p_write_error", req.data, "peer=%s err=%v", peer.ID, err)
			}
			util.LogModuleWarn("p2p", "[P2P-DIAG] write error for peer.ID=%s proxy=%s nodeID=%s status=%s frameType=0x%02x: %v",
				peer.ID, peer.ProxyNameSafe(), peer.NodeID, peer.GetStatus(), req.frameType, err)
			peer.LastError = err.Error()
			peer.transport.Close()
			return false
		}
		if req.frameType == frame.FrameMeshPacket {
			mesh.TraceForwarding("p2p_write_ok", req.data, "peer=%s", peer.ID)
		}
		return true
	}

	// writeBatch writes multiple data frames as a batch.
	writeBatch := func(batch []writeReq) bool {
		if len(batch) == 0 {
			return true
		}
		if len(batch) == 1 {
			return writeSingleFrame(batch[0], false)
		}

		// Encode frames for batch sending
		frames := make([]frame.Frame, len(batch))
		for i, req := range batch {
			frames[i] = frame.Frame{Type: req.frameType, Payload: req.data}
			if req.frameType == frame.FrameMeshPacket {
				mesh.TraceForwarding("p2p_write_start", req.data, "peer=%s batch=%d", peer.ID, len(batch))
				// Diagnostic: log first 20 bytes of each packet to identify IPIP
				if len(req.data) >= 20 {
					util.LogModuleDebug("p2p", "[P2P-WRITE-DIAG] peer=%s len=%d ver=%d proto=%d bytes=%x", 
						peer.ID, len(req.data), req.data[0]>>4, req.data[9], req.data[:20])
				}
			}
		}

		if err := peer.transport.SendBatch(frames); err != nil {
			for _, req := range batch {
				if req.frameType == frame.FrameMeshPacket {
					mesh.TraceForwarding("p2p_write_error", req.data, "peer=%s err=%v", peer.ID, err)
				}
			}
			util.LogModuleWarn("p2p", "[P2P] batch write error for %s: %v", peer.ID, err)
			peer.transport.Close()
			return false
		}

		for _, req := range batch {
			if req.frameType == frame.FrameMeshPacket {
				mesh.TraceForwarding("p2p_write_ok", req.data, "peer=%s batch=%d", peer.ID, len(batch))
			}
		}
		return true
	}

	for {
		// Fast path: control frames first, non-blocking.
		select {
		case req := <-peer.controlCh:
			if !writeSingleFrame(req, req.isControl) {
				return
			}
			continue
		default:
		}

		// Fast path: DNS packets (UDP port 53), non-blocking.
		select {
		case req := <-peer.dnsQueue:
			if !writeSingleFrame(req, req.isControl) {
				return
			}
			continue
		default:
		}

		// Fast path: TCP ACK packets, non-blocking.
		select {
		case req := <-peer.ackQueue:
			if !writeSingleFrame(req, req.isControl) {
				return
			}
			continue
		default:
		}

		// Block until we have something to send.
		// Priority: controlCh > dnsQueue > ackQueue > drrOutput (data).
		// No default branch — select blocks until data is available.
		select {
		case <-peer.stopCh:
			return
		case req := <-peer.controlCh:
			if !writeSingleFrame(req, req.isControl) {
				return
			}
		case req := <-peer.dnsQueue:
			if !writeSingleFrame(req, req.isControl) {
				return
			}
		case req := <-peer.ackQueue:
			if !writeSingleFrame(req, req.isControl) {
				return
			}
		case req, ok := <-peer.drrOutput:
			if !ok {
				return
			}
			// Diagnostic: log what we read from drrOutput
			if req.frameType == frame.FrameMeshPacket && len(req.data) >= 20 {
				util.LogModuleDebug("p2p", "[P2P-READ-DIAG] peer=%s len=%d ver=%d proto=%d", 
					peer.ID, len(req.data), req.data[0]>>4, req.data[9])
			}
			// Coalesce more data frames from drrOutput (non-blocking)
			batch := make([]writeReq, 0, maxBatchFrames)
			batch = append(batch, req)
			for len(batch) < maxBatchFrames {
				select {
				case more := <-peer.drrOutput:
					batch = append(batch, more)
				default:
					goto sendBatch
				}
			}
		sendBatch:
			// Diagnostic: log batch contents before writing
			for _, b := range batch {
				if b.frameType == frame.FrameMeshPacket && len(b.data) >= 20 {
					util.LogModuleDebug("p2p", "[P2P-BATCH-DIAG] peer=%s len=%d ver=%d proto=%d batchSize=%d",
						peer.ID, len(b.data), b.data[0]>>4, b.data[9], len(batch))
				}
			}
			if !writeBatch(batch) {
				return
			}
		}
	}
}

// drrLoop runs the DRR fair scheduler in a dedicated goroutine.
// Reads from dataQueues[256] using DRR algorithm, sends to drrOutput.
// v2: batch dequeue to reduce channel overhead and queue iteration.
// Exits when stopCh is closed.
func (m *P2PManager) drrLoop(peer *Peer) {
	for {
		// Batch dequeue: get up to 64 packets in one iteration
		batch := peer.drrScheduler.NextBatch(64, peer.stopCh)
		if len(batch) == 0 {
			return // stopped
		}
		
		// Send batch to drrOutput channel
		for _, req := range batch {
			select {
			case <-peer.stopCh:
				return
			case peer.drrOutput <- req:
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
				mesh.TraceForwarding("mesh_handler", pkt.frame, "fromNode=%s", pkt.fromNodeID)
				m.meshHandler.HandleMeshFrame(pkt.fromNodeID, pkt.linkNIC, pkt.frame)
			}
			// Return buffer to pool after processing (issue 10)
			frame.ReturnBuffer(pkt.frame)
		}
	}
}

// ErrPeerStopped is returned when sending to a peer that has been stopped.
var ErrPeerStopped = fmt.Errorf("peer stopped")

// ErrQueueFull is returned when the peer's write queue is full.
var ErrQueueFull = fmt.Errorf("write queue full")

// enqueueWrite queues a frame for async write. Mesh data is best-effort;
// control frames use a priority queue and retain their own ACK/retry semantics.
func enqueueWrite(peer *Peer, frameType byte, data []byte, isControl bool) error {
	return enqueueWriteReq(peer, writeReq{frameType: frameType, data: data, isControl: isControl})
}

func enqueueWriteReq(peer *Peer, req writeReq) error {
	select {
	case <-peer.stopCh:
		util.LogModuleDebug("p2p", "[P2P-ENQUEUE-DIAG] peer %s stopped, rejecting frame type=0x%02x", peer.ID, req.frameType)
		return ErrPeerStopped
	default:
	}

	// Select channel based on priority:
	// 1. Control frames -> controlCh (highest priority)
	// 2. DNS packets (UDP port 53) -> dnsQueue (second highest)
	// 3. TCP ACK packets -> ackQueue (third highest)
	// 4. All other data -> dataQueues[flowHash] (DRR fair scheduling)
	var ch chan writeReq
	isDNS := false
	isACK := false
	dataQueueIdx := -1
	
	if req.isControl {
		ch = peer.controlCh
	} else if isDNSPacket(req.data) {
		ch = peer.dnsQueue
		isDNS = true
	} else if isACKPacket(req.data) {
		ch = peer.ackQueue
		isACK = true
	} else {
		// All data packets: use DRR fair scheduling
		dataQueueIdx = peer.drrScheduler.hashFlow(req.data)
		ch = peer.dataQueues[dataQueueIdx]
	}

	// RED (Random Early Detection) for data frames only
	// Control frames are never dropped to ensure ACKs get through
	if !req.isControl {
		queueLen := len(ch)
		capacity := cap(ch)
		
		// RED parameters
		minThreshold := capacity * 50 / 100  // 50% capacity
		maxThreshold := capacity * 80 / 100  // 80% capacity
		
		if queueLen >= maxThreshold {
			// Queue almost full: drop with high probability (80%)
			if queueLen >= capacity-1 || (queueLen >= maxThreshold && peer.randomDrop(80)) {
				if req.frameType == frame.FrameMeshPacket {
					util.LogModuleWarn("p2p", "[P2P-RED] DROP peer=%s dataLen=%d queueLen=%d/%d priority=%s reason=max_threshold", 
						peer.ID, len(req.data), queueLen, capacity, channelPriorityName(ch, peer))
					mesh.TraceForwarding("p2p_red_drop", req.data, "peer=%s reason=max_threshold", peer.ID)
				}
				if isDNS {
					atomic.AddUint64(&peer.dnsQueueDrops, 1)
				} else if isACK {
					atomic.AddUint64(&peer.ackQueueDrops, 1)
				} else if dataQueueIdx >= 0 {
					atomic.AddUint64(&peer.dataQueueDrops[dataQueueIdx], 1)
				}
				return ErrQueueFull
			}
		} else if queueLen >= minThreshold {
			// Queue between min and max: drop with increasing probability (0-50%)
			// Linear increase: probability = (queueLen - minThreshold) / (maxThreshold - minThreshold) * 50%
			dropProbability := (queueLen - minThreshold) * 50 / (maxThreshold - minThreshold)
			if peer.randomDrop(dropProbability) {
				if req.frameType == frame.FrameMeshPacket {
					util.LogModuleDebug("p2p", "[P2P-RED] DROP peer=%s dataLen=%d queueLen=%d/%d priority=%s probability=%d%%", 
						peer.ID, len(req.data), queueLen, capacity, channelPriorityName(ch, peer), dropProbability)
					mesh.TraceForwarding("p2p_red_drop", req.data, "peer=%s probability=%d", peer.ID, dropProbability)
				}
				if isDNS {
					atomic.AddUint64(&peer.dnsQueueDrops, 1)
				} else if isACK {
					atomic.AddUint64(&peer.ackQueueDrops, 1)
				} else if dataQueueIdx >= 0 {
					atomic.AddUint64(&peer.dataQueueDrops[dataQueueIdx], 1)
				}
				return ErrQueueFull
			}
		}
	}

	select {
	case ch <- req:
		if req.frameType == frame.FrameMeshPacket {
			util.LogModuleDebug("p2p", "[P2P-ENQUEUE-DIAG] enqueued FrameMeshPacket peer=%s dataLen=%d chLen=%d priority=%s", 
				peer.ID, len(req.data), len(ch), channelPriorityName(ch, peer))
			mesh.TraceForwarding("p2p_enqueued", req.data, "peer=%s", peer.ID)
		}
		// Notify DRR scheduler if this is a data queue
		if dataQueueIdx >= 0 {
			peer.drrScheduler.Notify()
		}
		return nil
	default:
		if req.frameType == frame.FrameMeshPacket {
			util.LogModuleWarn("p2p", "[P2P-ENQUEUE-DIAG] QUEUE FULL peer=%s dataLen=%d chCap=%d priority=%s", 
				peer.ID, len(req.data), cap(ch), channelPriorityName(ch, peer))
			mesh.TraceForwarding("p2p_queue_drop", req.data, "peer=%s", peer.ID)
		}
		if req.isControl {
			atomic.AddUint64(&peer.controlChDrops, 1)
		} else if isDNS {
			atomic.AddUint64(&peer.dnsQueueDrops, 1)
		} else if isACK {
			atomic.AddUint64(&peer.ackQueueDrops, 1)
		} else if dataQueueIdx >= 0 {
			atomic.AddUint64(&peer.dataQueueDrops[dataQueueIdx], 1)
		}
		util.LogModuleDebug("p2p", "[P2P] write queue full for %s, dropping frame type=0x%02x", peer.ID, req.frameType)
		return ErrQueueFull
	}
}

// randomDrop returns true with the given probability (0-100)
func (p *Peer) randomDrop(probability int) bool {
	if probability <= 0 {
		return false
	}
	if probability >= 100 {
		return true
	}
	p.rngMu.Lock()
	result := p.rng.Intn(100) < probability
	p.rngMu.Unlock()
	return result
}

// isDNSPacket checks if a mesh packet contains UDP traffic to/from port 53 (DNS)
func isDNSPacket(data []byte) bool {
	// Need at least IPv4 header (20 bytes) + UDP header (8 bytes)
	if len(data) < 28 {
		return false
	}
	
	// Check IPv4 version
	if data[0]>>4 != 4 {
		return false
	}
	
	// Check protocol is UDP (17)
	if data[9] != 17 {
		return false
	}
	
	// Get IP header length (IHL field in bytes 0-3, lower 4 bits)
	ihl := int(data[0]&0x0F) * 4
	if ihl < 20 || len(data) < ihl+8 {
		return false
	}
	
	// Parse UDP source and destination ports
	srcPort := int(data[ihl])<<8 | int(data[ihl+1])
	dstPort := int(data[ihl+2])<<8 | int(data[ihl+3])
	
	// DNS uses port 53
	return srcPort == 53 || dstPort == 53
}

// isACKPacket checks if a packet contains a TCP ACK with small or no payload.
// These are prioritized to prevent TCP stalls from head-of-line blocking.
func isACKPacket(data []byte) bool {
	// Need at least IPv4 header (20 bytes) + TCP header (20 bytes min)
	if len(data) < 40 {
		return false
	}
	
	// Check IPv4 version
	if data[0]>>4 != 4 {
		return false
	}
	
	// Check protocol is TCP (6)
	if data[9] != 6 {
		return false
	}
	
	// Get IP header length
	ihl := int(data[0]&0x0F) * 4
	if ihl < 20 || len(data) < ihl+20 {
		return false
	}
	
	// TCP data offset (header length in 32-bit words)
	tcpDataOffset := int(data[ihl+12]>>4) * 4
	if tcpDataOffset < 20 {
		return false
	}
	
	// TCP flags are at offset 13 in TCP header
	flags := data[ihl+13]
	// Must have ACK bit set
	if flags&0x10 == 0 {
		return false
	}
	// Exclude SYN, FIN, RST (pure ACK or ACK+PSH only)
	if flags&0x02 != 0 || flags&0x01 != 0 || flags&0x04 != 0 {
		return false
	}
	
	// Check TCP payload is small (< 100 bytes)
	tcpPayloadLen := len(data) - ihl - tcpDataOffset
	return tcpPayloadLen < 100
}

// hashFlow computes a flow hash for fair scheduling using Jenkins one-at-a-time.
// Uses 5-tuple (srcIP, dstIP, protocol, srcPort, dstPort) for TCP/UDP,
// falls back to 3-tuple (srcIP, dstIP, protocol) for other protocols.
// v2: Jenkins hash with random salt for better distribution.
func (s *DRRScheduler) hashFlow(data []byte) int {
	if len(data) < 20 {
		return 0
	}
	
	// Check IPv4
	if data[0]>>4 != 4 {
		return 0
	}
	
	protocol := data[9]
	ihl := int(data[0]&0x0F) * 4
	if ihl < 20 {
		ihl = 20
	}
	
	// Jenkins one-at-a-time hash with salt
	h := s.salt
	
	// Base hash: srcIP + dstIP + protocol
	for i := 12; i < 20; i++ {
		h += uint32(data[i])
		h += h << 10
		h ^= h >> 6
	}
	h += uint32(protocol)
	h += h << 10
	h ^= h >> 6
	
	// Add ports only for TCP (6) and UDP (17)
	if (protocol == 6 || protocol == 17) && len(data) >= ihl+4 {
		for i := ihl; i < ihl+4; i++ {
			h += uint32(data[i])
			h += h << 10
			h ^= h >> 6
		}
	}
	
	// Final mixing
	h += h << 3
	h ^= h >> 11
	h += h << 15
	
	return int(h % 256)
}

// channelPriorityName returns the priority name for logging
func channelPriorityName(ch chan writeReq, peer *Peer) string {
	if ch == peer.controlCh {
		return "control"
	} else if ch == peer.dnsQueue {
		return "dns"
	} else if ch == peer.ackQueue {
		return "ack"
	}
	// Check if it's a data queue
	for i := range peer.dataQueues {
		if ch == peer.dataQueues[i] {
			return fmt.Sprintf("data-%d", i)
		}
	}
	return "unknown"
}

// HandleP2PTransport runs a P2P session over an established frame transport
// (server side). The transport is owned by the manager and closed when the
// session ends.
func (m *P2PManager) HandleP2PTransport(t frame.FrameTransport, address string) {
	if !m.beginSession() {
		t.Close()
		return
	}
	defer m.sessionsWG.Done()

	seq := uint16(atomic.AddUint64(&m.linkSeqCounter, 1))
	
	// Initialize data queues for DRR fair scheduling
	var dataQueues [256]chan writeReq
	for i := range dataQueues {
		dataQueues[i] = make(chan writeReq, 32) // v2: 32 packets per queue (was 256)
	}
	
	peer := &Peer{
		ID:             fmt.Sprintf("%d", seq),
		LastSeen:       time.Now(),
		transport:      t,
		dnsQueue:       make(chan writeReq, 512),
		ackQueue:       make(chan writeReq, 256),
		dataQueues:     dataQueues,
		drrOutput:      make(chan writeReq, 512),
		controlCh:      make(chan writeReq, 512),
		stopCh:         make(chan struct{}),
		sendState:      newSendState(),
		recvState:      newRecvState(),
		Seq:            seq,
		drrScheduler:   newDRRScheduler(dataQueues),
	}
	peer.rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	peer.lastSeenNano.Store(time.Now().UnixNano())
	peer.SetStatus("connecting")

	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		stopPeer(peer)
		return
	}
	m.peers[peer.ID] = peer
	m.mu.Unlock()

	defer func() {
		stopPeer(peer)
		m.mu.Lock()
		if sender := peer.GetMeshSender(); sender != nil && m.meshHandler != nil {
			m.meshHandler.UnregisterPeer(sender)
		}
		// Delete LinkNIC when peer disconnects (design §5.6)
		if linkNIC := peer.GetLinkNIC(); linkNIC != nil && m.meshHandler != nil {
			m.meshHandler.DeleteLinkNIC(linkNIC)
			peer.SetLinkNIC(nil)
		}
		// Mark all pending frames as lost before saving to linkStats.
		// This ensures frames in-flight during disconnection count in both loss rate and latency statistics.
		if peer.sendState != nil {
			peer.sendState.markAllPendingAsLost()
		}
		if current, ok := m.peers[peer.ID]; ok && current == peer {
			delete(m.peers, peer.ID)
		}
		m.mu.Unlock()
		util.LogModuleInfo("p2p", "[P2P] session ended for %s", peer.ID)
	}()

	go m.drrLoop(peer)
	go m.peerWriteLoop(peer)
	m.runSession(peer)
}

// StopPeerByProxyName disconnects one P2P session by its configured proxy name.
func (m *P2PManager) StopPeerByProxyName(proxyName string) {
	m.mu.Lock()
	peer := m.proxyPeers[proxyName]
	if peer != nil {
		delete(m.proxyPeers, proxyName)
		delete(m.peers, peer.ID)
	}
	m.mu.Unlock()
	if peer != nil {
		util.LogModuleInfo("p2p", "[P2P] stopping peer for proxy %s (peer.ID=%s)", proxyName, peer.ID)
		stopPeer(peer)
	}
}

// ActivePeerProxyNames returns the set of proxy names that currently have active peers.
// Passive (inbound) peers have no ProxyName and are excluded.
func (m *P2PManager) ActivePeerProxyNames() map[string]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make(map[string]bool, len(m.proxyPeers))
	for name := range m.proxyPeers {
		result[name] = true
	}
	return result
}

// StopPeerByLinkID disconnects every session with the exact negotiated link ID.
func (m *P2PManager) StopPeerByLinkID(linkID string) {
	m.mu.Lock()
	peers := make([]*Peer, 0)
	for _, peer := range m.peers {
		if peer.LinkID == linkID {
			peers = append(peers, peer)
		}
	}
	m.mu.Unlock()

	for _, peer := range peers {
		util.LogModuleInfo("p2p", "[P2P] stopping peer %s (linkID=%s) due to connectivity failure", peer.ID, linkID)
		stopPeer(peer)
	}
}

// GetPeerProxy returns the proxy config that a peer was started with.
// Returns nil if the peer does not exist.
func (m *P2PManager) GetPeerProxy(proxyName string) *config.Proxy {
	m.mu.Lock()
	defer m.mu.Unlock()
	if peer, ok := m.proxyPeers[proxyName]; ok {
		return peer.proxy
	}
	return nil
}

// RestartPeer stops an existing peer and starts a new one with updated config.
// This is used when proxy config (password, server, etc.) changes.
func (m *P2PManager) RestartPeer(proxy *config.Proxy) {
	util.LogModuleInfo("p2p", "[P2P] restarting peer for proxy %s due to config change", proxy.Name)
	m.StopPeerByProxyName(proxy.Name)
	go m.StartPeer(proxy)
}

// StartPeer initiates a P2P connection to a peer through the given proxy.
// It reconnects automatically with exponential backoff if the connection drops.
// Call StopPeerByProxyName to permanently disconnect.
func (m *P2PManager) StartPeer(proxy *config.Proxy) {
	if !m.beginSession() {
		util.LogModuleDebug("p2p", "[P2P] manager stopped; not starting peer %s", proxy.Name)
		return
	}
	defer m.sessionsWG.Done()

	util.LogModuleInfo("p2p", "[P2P] StartPeer called for proxy %s (type=%s, p2p=%v)", proxy.Name, proxy.Type, proxy.IsP2P())

	seq := uint16(atomic.AddUint64(&m.linkSeqCounter, 1))
	
	// Initialize data queues for DRR fair scheduling
	var dataQueues [256]chan writeReq
	for i := range dataQueues {
		dataQueues[i] = make(chan writeReq, 32) // v2: 32 packets per queue (was 256)
	}
	
	peer := &Peer{
		ID:           fmt.Sprintf("%d", seq),
		ProxyName:    proxy.Name,
		LastSeen:     time.Now(),
		stopCh:       make(chan struct{}),
		sessionDone:  make(chan struct{}),
		proxy:        proxy,
		Seq:          seq,
		dnsQueue:     make(chan writeReq, 512),
		ackQueue:     make(chan writeReq, 256),
		dataQueues:   dataQueues,
		drrOutput:    make(chan writeReq, 512),
		controlCh:    make(chan writeReq, 512),
		drrScheduler: newDRRScheduler(dataQueues),
	}
	peer.rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	peer.lastSeenNano.Store(time.Now().UnixNano())
	peer.SetStatus("connecting")

	// Atomic check-stop-create-register using proxyPeers for O(1) dedup
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		stopPeer(peer)
		return
	}
	
	// Check for existing peer with same proxy name (O(1) lookup)
	if existing, ok := m.proxyPeers[proxy.Name]; ok {
		util.LogModuleInfo("p2p", "[P2P] stopping existing peer %s for proxy %s before starting new one", existing.ID, proxy.Name)
		// CRITICAL: Unregister existing peer's sender from topology BEFORE stopping.
		// This prevents stale PeerInfo entries from accumulating when peers reconnect rapidly.
		// The defer in the existing peer's session will run asynchronously, but we need to
		// clean up the topology synchronously to avoid race conditions.
		if sender := existing.GetMeshSender(); sender != nil && m.meshHandler != nil {
			m.meshHandler.UnregisterPeer(sender)
			existing.SetMeshSender(nil) // Prevent defer from unregistering again
		}
		stopPeer(existing)
		delete(m.proxyPeers, proxy.Name)
		delete(m.peers, existing.ID)
	}
	
	// Register new peer atomically in both maps
	m.peers[peer.ID] = peer
	m.proxyPeers[proxy.Name] = peer
	m.mu.Unlock()

	defer func() {
		stopPeer(peer)
		m.mu.Lock()
		// DON'T UnregisterPeer here! The inner defer (line 1350) already handles it.
		// Since defers are LIFO, the inner defer runs first and calls UnregisterPeer.
		// If we also call it here, it would be a no-op (sender already removed from topology),
		// but it's cleaner to let the inner defer handle all mesh cleanup.
		
		// Delete LinkNIC when peer disconnects (design §5.6)
		// Note: Inner defer also does this, but it's safe to call DeleteLinkNIC twice
		// (second call is a no-op since LinkNIC is already deleted).
		if linkNIC := peer.GetLinkNIC(); linkNIC != nil && m.meshHandler != nil {
			m.meshHandler.DeleteLinkNIC(linkNIC)
			peer.SetLinkNIC(nil)
		}
		// Mark all pending frames as lost.
		// This ensures frames in-flight during disconnection count in both loss rate and latency statistics.
		if peer.sendState != nil {
			peer.sendState.markAllPendingAsLost()
		}
		// Remove from both maps (prevent race with RestartPeer)
		if current, ok := m.peers[peer.ID]; ok && current == peer {
			delete(m.peers, peer.ID)
		}
		if current, ok := m.proxyPeers[proxy.Name]; ok && current == peer {
			delete(m.proxyPeers, proxy.Name)
		}
		m.mu.Unlock()
		// Signal that session has fully ended (including LinkNIC cleanup)
		close(peer.sessionDone)
	}()

	d := dialer.NewDialer(proxy)
	p2pDialer, ok := d.(dialer.P2PDialer)
	if !ok {
		util.LogModuleInfo("p2p", "[P2P] proxy %s (%s) does not support P2P", proxy.Name, proxy.Type)
		return
	}

	backoff := time.Second
	const maxBackoff = 60 * time.Second

	for {
		// Check if peer has been stopped before attempting to connect
		select {
		case <-peer.stopCh:
			util.LogModuleInfo("p2p", "[P2P] peer %s stopped, exiting reconnect loop", peer.ID)
			return
		default:
		}

		transport, err := p2pDialer.DialP2P()
		if err != nil {
			util.LogModuleInfo("p2p", "[P2P] failed to connect to %s via proxy %s: %v", proxy.Server, proxy.Name, err)
			peer.LastError = err.Error()
			peer.SetStatus("failed")
			m.notifyStatusChange()
			// Add jitter: delay = backoff * (0.5 + rand[0,1)) = backoff * [0.5, 1.5)
			peer.rngMu.Lock()
			jitter := 0.5 + peer.rng.Float64()
			peer.rngMu.Unlock()
			jitteredBackoff := time.Duration(float64(backoff) * jitter)
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
		peer.dnsQueue = make(chan writeReq, 512)
		peer.ackQueue = make(chan writeReq, 256)
		peer.controlCh = make(chan writeReq, 512)
		peer.drrOutput = make(chan writeReq, 512)
		peer.rng = rand.New(rand.NewSource(time.Now().UnixNano()))
		// Reinitialize data queues for DRR fair scheduling
		for i := range peer.dataQueues {
			peer.dataQueues[i] = make(chan writeReq, 32) // v2: 32 packets per queue (was 256)
		}
		if peer.drrScheduler != nil {
			peer.drrScheduler.queues = peer.dataQueues
			peer.drrScheduler.deficits = [256]int{}
			peer.drrScheduler.round = 0
		}
		// Create sendState/recvState on first connection attempt.
		// Note: These are NOT preserved across reconnections (line 1178 creates a new peer object).
		// The nil check only matters when retrying failed dial attempts within the same StartPeer invocation.
		if peer.sendState == nil {
			peer.sendState = newSendState()
		}
		if peer.recvState == nil {
			peer.recvState = newRecvState()
		}
		peer.SetStatus("connecting")
		m.notifyStatusChange()
		util.LogModuleInfo("p2p", "[P2P] connected to %s via proxy %s", proxy.Server, proxy.Name)

		// Run session in a closure so we can use defer for cleanup
		sessionStartTime := time.Now()
		func() {
			defer func() {
				sessionDuration := time.Since(sessionStartTime)
				// Log disconnection reason
				util.LogModuleInfo("p2p", "[P2P-DIAG] session ending for peer.ID=%s proxy=%s status=%s lastError=%v duration=%v",
					peer.ID, proxy.Name, peer.GetStatus(), peer.LastError, sessionDuration)
				
				transport.Close()
				
				// Update status to disconnected before cleanup
				if peer.GetStatus() != "stopped" {
					peer.SetStatus("disconnected")
					m.notifyStatusChange()
				}
				
				// Clean up mesh peer registration
				m.mu.Lock()
				if sender := peer.GetMeshSender(); sender != nil && m.meshHandler != nil {
					m.meshHandler.UnregisterPeer(sender)
				}
				// Delete LinkNIC when peer disconnects (design §5.6)
				if linkNIC := peer.GetLinkNIC(); linkNIC != nil && m.meshHandler != nil {
					m.meshHandler.DeleteLinkNIC(linkNIC)
					peer.SetLinkNIC(nil)
				}
				peer.SetMeshSender(nil)
				m.mu.Unlock()
			}()

			go m.drrLoop(peer)
			go m.peerWriteLoop(peer)
			m.runSession(peer)
		}()

		// Session ended. Instead of reusing the same peer object (stale
		// seq/ID), stop this peer and spawn a fresh StartPeer so the new
		// connection gets a brand-new seq, peer.ID, and clean state.
		if peer.GetStatus() != "stopped" {
			proxy := peer.proxy
			sessionDuration := time.Since(sessionStartTime)
			
			// If session was very short (< 5s), apply backoff to prevent rapid reconnection loops
			if sessionDuration < 5*time.Second {
				util.LogModuleWarn("p2p", "[P2P-DIAG] session too short (%v) for peer.ID=%s proxy=%s, applying backoff before reconnect",
					sessionDuration, peer.ID, proxy.Name)
				
				// Exponential backoff with jitter
				peer.rngMu.Lock()
				jitter := 0.5 + peer.rng.Float64()
				peer.rngMu.Unlock()
				jitteredBackoff := time.Duration(float64(backoff) * jitter)
				
				select {
				case <-peer.stopCh:
					return
				case <-time.After(jitteredBackoff):
				}
				
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			} else {
				// Reset backoff for long-lived sessions
				backoff = time.Second
			}
			
			util.LogModuleInfo("p2p", "[P2P] disconnected from %s (proxy=%s status=%s duration=%v), starting fresh peer",
				peer.ID, proxy.Name, peer.GetStatus(), sessionDuration)
			go m.StartPeer(proxy)
		}
		return
	}
}

// runSession reads frames and dispatches them.
func (m *P2PManager) runSession(peer *Peer) {
	// Log session start with full context (peer.proxy may be nil for passive/inbound sessions)
	proxyName := peer.ProxyNameSafe()
	util.LogModuleInfo("p2p", "[P2P-DIAG] runSession START for peer.ID=%s proxy=%s nodeID=%s status=%s",
		peer.ID, proxyName, peer.NodeID, peer.GetStatus())
	
	// Send hello immediately
	util.LogModuleInfo("p2p", "[P2P-DIAG] runSession calling sendHello for peer.ID=%s proxy=%s", peer.ID, proxyName)
	m.sendHello(peer)
	util.LogModuleInfo("p2p", "[P2P-DIAG] runSession sendHello completed for peer.ID=%s", peer.ID)

	util.LogModuleInfo("p2p", "[P2P] runSession started for %s (proxy=%s)", peer.ID, proxyName)

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
				lastSeenNano := peer.lastSeenNano.Load()
				lastSeenTime := time.Unix(0, lastSeenNano)
				if time.Since(lastSeenTime) > heartbeatTimeout {
					util.LogModuleWarn("p2p", "[P2P-DIAG] heartbeat timeout for peer.ID=%s proxy=%s nodeID=%s status=%s (last seen %v ago), closing connection",
						peer.ID, peer.ProxyNameSafe(), peer.NodeID, peer.GetStatus(), time.Since(lastSeenTime))
					peer.LastError = fmt.Sprintf("heartbeat timeout: no data for %v", time.Since(lastSeenTime))
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
		// Return pooled buffer at end of iteration (issue 10)
		// Note: For mesh packets passed to meshInboundCh, the consumer will return it
		originalPayload := payload
		payloadReturned := false
		
		now := time.Now()
		peer.LastSeen = now
		peer.lastSeenNano.Store(now.UnixNano())
		if err != nil {
			// Log detailed disconnection reason
			util.LogModuleInfo("p2p", "[P2P-DIAG] read error for peer.ID=%s proxy=%s nodeID=%s status=%s: %v",
				peer.ID, peer.ProxyNameSafe(), peer.NodeID, peer.GetStatus(), err)
			peer.LastError = err.Error()
			frame.ReturnBuffer(originalPayload)
			return
		}
		util.LogModuleDebug("p2p", "[P2P] received frame type=0x%02x len=%d from %s", frameType, len(payload), peer.ID)

		// Control frames have seq/ack encoded in the first 8 bytes
		isControl := frameType == frame.FrameHello || frameType == frame.FrameGossip || frameType == frame.FrameAck
		if isControl {
			if len(payload) < 8 {
				util.LogModuleWarn("p2p", "[P2P] control frame too short from %s: len=%d", peer.ID, len(payload))
				frame.ReturnBuffer(originalPayload)
				payloadReturned = true
				continue
			}
			seq := binary.BigEndian.Uint32(payload[0:4])
			ack := binary.BigEndian.Uint32(payload[4:8])
			payload = payload[8:] // strip seq/ack header

			// Process ack (removes acknowledged frames from pending)
			peer.sendState.processAck(ack)

			// Pure ACK frame: no further processing needed
			if frameType == frame.FrameAck {
				frame.ReturnBuffer(originalPayload)
				payloadReturned = true
				continue
			}

			// Check for duplicate
			if !peer.recvState.checkAndRecord(seq) {
				util.LogModuleDebug("p2p", "[P2P] duplicate control frame from %s: seq=%d", peer.ID, seq)
				frame.ReturnBuffer(originalPayload)
				payloadReturned = true
				continue
			}

			// Send immediate pure ACK for Hello/Gossip frames
			m.sendPureAck(peer)
		}

		switch frameType {
		case frame.FrameHello:
			util.LogModuleInfo("p2p", "[P2P-DIAG] received FrameHello from %s, payload=%d bytes", peer.ID, len(payload))
			if len(payload) > 0 {
				m.handleHello(peer, payload)
			} else {
				util.LogModuleWarn("p2p", "[P2P-DIAG] FrameHello from %s has empty payload", peer.ID)
			}
		case frame.FrameGossip:
			if len(payload) > 0 {
				m.handleGossip(peer, payload)
			}
		case frame.FrameMeshPacket:
			if m.meshHandler != nil && len(payload) > 0 {
				mesh.TraceForwarding("p2p_received", payload, "peer=%s node=%s", peer.ID, peer.NodeID)
				linkNIC := peer.GetLinkNIC()
				if linkNIC == nil {
					util.LogModuleWarn("p2p", "[P2P-DIAG] FrameMeshPacket: peer.GetLinkNIC() is nil! peer.ID=%s node=%s status=%s", peer.ID, peer.NodeID, peer.GetStatus())
				}
				// Zero-copy: pass payload directly to mesh layer.
				// Consumer (meshInboundLoop) MUST call frame.ReturnBuffer when done (issue 10).
				select {
				case m.meshInboundCh <- meshInboundPacket{fromNodeID: peer.NodeID, linkNIC: linkNIC, frame: payload}:
					mesh.TraceForwarding("p2p_inbound_enqueued", payload, "peer=%s node=%s", peer.ID, peer.NodeID)
					payloadReturned = true  // Consumer will return it
				default:
					atomic.AddUint64(&m.meshInboundDrops, 1)
					mesh.TraceForwarding("p2p_inbound_drop", payload, "peer=%s node=%s", peer.ID, peer.NodeID)
					util.LogModuleDebug("p2p", "[P2P] meshInboundCh full, dropping frame from %s", peer.NodeID)
				}
			} else {
				util.LogModuleWarn("p2p", "[P2P] FrameMeshPacket dropped: meshHandler=%v payloadLen=%d", m.meshHandler != nil, len(payload))
			}
		default:
			util.LogModuleInfo("p2p", "[P2P] unexpected frame type 0x%02x from %s (%d bytes)", frameType, peer.ID, len(payload))
		}
		
		// Return buffer if not passed to consumer
		if !payloadReturned {
			frame.ReturnBuffer(originalPayload)
		}
	}
}

// checkRetransmissions checks for timed-out control frames and retransmits them.
// Uses exponential backoff: RTO, 2*RTO, 4*RTO, 8*RTO, 16*RTO (capped at 60s), max 5 retries.
func (m *P2PManager) checkRetransmissions(peer *Peer) {
	baseRTO := peer.sendState.rttSampler.getRTO()
	pending := peer.sendState.getRetransmissions(baseRTO)

	for _, pf := range pending {
		if pf.retries >= 5 {
			// Give up after 5 retries
			util.LogModuleWarn("p2p", "[P2P] giving up on control frame to %s: seq=%d retries=%d", peer.ID, pf.seq, pf.retries)
			peer.sendState.removeFrame(pf.seq)
			continue
		}

		// Calculate timeout for this retry (for logging)
		timeout := baseRTO * time.Duration(1<<pf.retries)
		if timeout > 60*time.Second {
			timeout = 60 * time.Second
		}
		util.LogModuleDebug("p2p", "[P2P] retransmitting control frame to %s: seq=%d retries=%d timeout=%v", peer.ID, pf.seq, pf.retries, timeout)

		// Re-encode with current ack
		ack := peer.recvState.getLastSeq()
		encoded := make([]byte, 8+len(pf.payload))
		binary.BigEndian.PutUint32(encoded[0:4], pf.seq)
		binary.BigEndian.PutUint32(encoded[4:8], ack)
		copy(encoded[8:], pf.payload)

		if err := enqueueWriteReq(peer, writeReq{
			frameType:      pf.frameType,
			data:           encoded,
			isControl:      true,
			encodedControl: true,
		}); err != nil {
			util.LogModuleDebug("p2p", "[P2P] retransmit queue unavailable for %s: %v", peer.ID, err)
			return
		}
		peer.sendState.markRetransmitted(pf.seq)
	}
}

// sendPureAck sends an immediate pure ACK frame to acknowledge received control frames.
// Pure ACK carries only the ack number and uses seq=0 (no acknowledgment expected for ACK itself).
// This ensures accurate RTT measurement by responding immediately instead of waiting for next gossip.
// The ACK is sent asynchronously through controlCh to avoid concurrent writes with peerWriteLoop.
func (m *P2PManager) sendPureAck(peer *Peer) {
	ack := peer.recvState.getLastSeq()
	if ack == 0 {
		return // Nothing to acknowledge
	}

	// Use pre-allocated buffer to avoid allocation per ACK
	binary.BigEndian.PutUint32(peer.ackBuf[:], ack)

	// Send through controlCh to avoid concurrent writes with peerWriteLoop
	// Mark as control frame so it gets priority and correct timeout
	select {
	case peer.controlCh <- writeReq{frameType: frame.FrameAck, data: peer.ackBuf[:], isControl: true}:
		// Queued successfully
	default:
		atomic.AddUint64(&peer.controlChDrops, 1)
		util.LogModuleDebug("p2p", "[P2P] control queue full, dropping pure ACK to %s", peer.ID)
	}
}

// sendHello sends a hello message to the peer, carrying gossip topology for immediate route establishment.
func (m *P2PManager) sendHello(peer *Peer) {
	util.LogModuleInfo("p2p", "[P2P-DIAG] sendHello called for peer=%s", peer.ID)
	info := mesh.GossipInfo{
		Cmd:             "hello",
		ProtocolVersion: P2PProtocolVersion,
		Seq:             peer.Seq, // Use peer's sequence number for link ID negotiation
	}

	// Set friendly name if this is an active (outbound) connection
	if peer.proxy != nil {
		info.FriendlyName = peer.proxy.Name
	}

	// Build gossip content from mesh handler
	if m.meshHandler != nil {
		gossipInfo := m.meshHandler.BuildGossipInfo()
		info.DomainSuffixes = gossipInfo.DomainSuffixes
		info.Routes = gossipInfo.Routes
		info.ClaimedSubnets = gossipInfo.ClaimedSubnets
	}

	data, _ := json.Marshal(info)
	util.LogModuleInfo("p2p", "[P2P-DIAG] sendHello enqueuing FrameHello for peer=%s dataLen=%d", peer.ID, len(data))
	enqueueWrite(peer, frame.FrameHello, data, true) // control frame, reliable delivery
}

// handleHello processes a hello message from a peer.
func (m *P2PManager) handleHello(peer *Peer, payload []byte) {
	util.LogModuleInfo("p2p", "[P2P-DIAG] handleHello entry: peer=%s payload=%d bytes", peer.ID, len(payload))
	var info mesh.GossipInfo
	if err := json.Unmarshal(payload, &info); err != nil {
		util.LogModuleDebug("p2p", "[P2P] invalid hello from %s: %v", peer.ID, err)
		return
	}
	util.LogModuleInfo("p2p", "[P2P-DIAG] handleHello parsed: peer=%s nodeID=%s subnets=%d version=%d", 
		peer.ID, extractNodeIDFromGossip(info), len(info.ClaimedSubnets), info.ProtocolVersion)

	// 1. Version check
	if info.ProtocolVersion != P2PProtocolVersion {
		util.LogModuleWarn("p2p", "[P2P] protocol version mismatch from %s: peer=%d local=%d, disconnecting",
			peer.ID, info.ProtocolVersion, P2PProtocolVersion)
		peer.transport.Close()
		return
	}

	// 2. Extract nodeID from ClaimedSubnets[hop=0]
	nodeID := extractNodeIDFromGossip(info)
	if nodeID == "" {
		util.LogModuleDebug("p2p", "[P2P] hello from %s has no claimed subnet with hop=0", peer.ID)
		peer.transport.Close()
		return
	}

	peer.NodeID = nodeID
	peer.LastError = ""
	peer.SetStatus("helloed")
	now := time.Now()
	peer.LastSeen = now
	peer.lastSeenNano.Store(now.UnixNano())
	
	// Store remote friendly name (proxy name)
	if info.FriendlyName != "" {
		peer.FriendlyName = info.FriendlyName
	}

	// 3. Link ID negotiation
	remoteSeq := info.Seq
	localSeq := peer.Seq
	peer.RemoteSeq = remoteSeq // Store for gossip advertisement

	if remoteSeq > 0 && localSeq > 0 {
		// Sort and concatenate to generate link ID (internal use only)
		minSeq := localSeq
		maxSeq := remoteSeq
		if localSeq > remoteSeq {
			minSeq = remoteSeq
			maxSeq = localSeq
		}
		peer.LinkID = fmt.Sprintf("%d-%d", minSeq, maxSeq)
	}

	m.notifyStatusChange()

	util.LogModuleInfo("p2p", "[P2P] hello from %s: nodeID=%s, linkID=%s", peer.ID, nodeID, peer.LinkID)

	// 4. Clean up old state + re-register
	if m.meshHandler != nil {
		// Unregister old sender for this specific peer connection.
		// Each proxy has its own link, so we unregister by sender pointer (not by nodeID)
		// to avoid affecting other links to the same remote node via different proxies.
		if sender := peer.GetMeshSender(); sender != nil {
			m.meshHandler.UnregisterPeer(sender)
		}
		ps := &peerSender{peer: peer, nodeID: nodeID}
		peer.SetMeshSender(ps)
		m.meshHandler.RegisterPeer(ps)
		
		// 5. Create LinkNIC for this link (design §5.6)
		// Extract subnet from ClaimedSubnets[hop=0]
		var subnet string
		for _, cs := range info.ClaimedSubnets {
			if cs.Hop == 0 {
				subnet = cs.Subnet
				break
			}
		}
		util.LogModuleInfo("p2p", "[P2P-DIAG] LinkNIC creation: peer=%s nodeID=%s subnet=%q", peer.ID, nodeID, subnet)
		if subnet != "" {
			linkNIC := m.meshHandler.CreateLinkNIC(ps, nodeID, subnet)
			peer.SetLinkNIC(linkNIC)
			if linkNIC == nil {
				util.LogModuleWarn("p2p", "[P2P] failed to create LinkNIC for peer %s (nodeID=%s)", peer.ID, nodeID)
			} else {
				util.LogModuleInfo("p2p", "[P2P-DIAG] LinkNIC created successfully: peer=%s nodeID=%s", peer.ID, nodeID)
			}
		} else {
			util.LogModuleWarn("p2p", "[P2P] no subnet in hello from %s, cannot create LinkNIC", peer.ID)
		}
	}

	// 6. Process topology (shared logic)
	m.processGossipInfo(peer, info)

	peer.SetStatus("upToDate")
	m.notifyStatusChange()
}

// hashString generates a simple hash from a string (for sequence number generation)
// handleGossip processes a gossip message from a peer.
func (m *P2PManager) handleGossip(peer *Peer, payload []byte) {
	// Validate JSON by unmarshaling (issue 7: avoid remarshaling)
	var info mesh.GossipInfo
	if err := json.Unmarshal(payload, &info); err != nil {
		util.LogModuleDebug("p2p", "[P2P] invalid gossip from %s: %v", peer.ID, err)
		return
	}

	// Forward raw payload directly to avoid unmarshal→marshal cycle (issue 7)
	if sender := peer.GetMeshSender(); m.meshHandler != nil && sender != nil {
		m.meshHandler.HandleTopologyGossip(sender, payload)
	}
}

// processGossipInfo is the unified topology processing logic for hello.
// For gossip, use handleGossip which forwards raw payload directly.
func (m *P2PManager) processGossipInfo(peer *Peer, info mesh.GossipInfo) {
	if sender := peer.GetMeshSender(); m.meshHandler != nil && sender != nil {
		data, _ := json.Marshal(info)
		m.meshHandler.HandleTopologyGossip(sender, data)
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
	// Update nodeId to match mesh nodeID for correct linkKey construction.
	// P2PManager is created before MeshManager, so nodeId is initially a placeholder.
	if nodeID != "" {
		m.nodeId = nodeID
	}
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
		if p.meshSenderPtr.Load() != nil {
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
		sender := p.meshSenderPtr.Load()
		if sender != nil && sender.nodeID == peerNodeID {
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
		if p.meshSenderPtr.Load() != nil {
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
	util.LogModuleDebug("p2p", "[P2P] resent hello to all peers (%d)", len(m.peers))
}

// ListMeshPeerIDs returns node IDs of all mesh-enabled peers.
func (m *P2PManager) ListMeshPeerIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	var result []string
	for _, p := range m.peers {
		sender := p.meshSenderPtr.Load()
		if sender != nil {
			result = append(result, sender.nodeID)
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
		lastSeenNano := p.lastSeenNano.Load()
		result = append(result, Peer{
			ID:        p.ID,
			ProxyName: p.ProxyName,
			NodeID:    p.NodeID,
			Status:    p.GetStatus(),
			LastSeen:  time.Unix(0, lastSeenNano),
		})
	}
	return result
}

// GetLinkQualityStats returns ACK-based link quality stats for a peer by nodeID.
// totalDataDrops returns the sum of all data queue drops (dns + ack + data[256]).
func (p *Peer) totalDataDrops() uint64 {
	var total uint64
	total += atomic.LoadUint64(&p.dnsQueueDrops)
	total += atomic.LoadUint64(&p.ackQueueDrops)
	for i := range p.dataQueueDrops {
		total += atomic.LoadUint64(&p.dataQueueDrops[i])
	}
	return total
}

// Returns srtt, rto, jitter, lossRate, sampleCount. Returns zeros if peer not found.
func (m *P2PManager) GetLinkQualityStats(nodeID string) (srtt, rto, jitter time.Duration, lossRate float64, sampleCount int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, p := range m.peers {
		if p.NodeID == nodeID && p.sendState != nil {
			channelDrops := p.totalDataDrops() + atomic.LoadUint64(&p.controlChDrops)
			return p.sendState.getStats(channelDrops)
		}
	}
	return 0, 0, 0, 0, 0
}

// GetChannelDropStats returns channel overflow drop statistics.
// Returns map[nodeID]{dataDrops, controlChDrops} for all peers, plus meshInboundDrops.
func (m *P2PManager) GetChannelDropStats() (peerStats map[string]struct {
	DataDrops      uint64
	ControlChDrops uint64
}, meshInboundDrops uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	peerStats = make(map[string]struct {
		DataDrops      uint64
		ControlChDrops uint64
	})
	for _, p := range m.peers {
		peerStats[p.NodeID] = struct {
			DataDrops      uint64
			ControlChDrops uint64
		}{
			DataDrops:      p.totalDataDrops(),
			ControlChDrops: atomic.LoadUint64(&p.controlChDrops),
		}
	}
	meshInboundDrops = atomic.LoadUint64(&m.meshInboundDrops)
	return
}


// GetLinkQualityStatsByProxy returns ACK-based link quality stats for a specific link (proxy).
// Returns srtt, rto, jitter, lossRate, sampleCount. Returns zeros if peer not found.
func (m *P2PManager) GetLinkQualityStatsByProxy(proxyName string) (srtt, rto, jitter time.Duration, lossRate float64, sampleCount int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, p := range m.peers {
		if p.ProxyName == proxyName && p.sendState != nil {
			channelDrops := p.totalDataDrops() + atomic.LoadUint64(&p.controlChDrops)
			return p.sendState.getStats(channelDrops)
		}
	}
	return 0, 0, 0, 0, 0
}

// GetLinkQualityStatsByLinkID returns ACK-based link quality stats for an exact negotiated link.
func (m *P2PManager) GetLinkQualityStatsByLinkID(linkID string) (srtt, rto, jitter time.Duration, lossRate float64, sampleCount int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, p := range m.peers {
		if p.LinkID == linkID && p.sendState != nil {
			channelDrops := p.totalDataDrops() + atomic.LoadUint64(&p.controlChDrops)
			return p.sendState.getStats(channelDrops)
		}
	}
	return 0, 0, 0, 0, 0
}

// GetAllLinkQualityStats returns quality stats for all links.
// Returns map[proxyName]{nodeID, srtt, rto, jitter, lossRate, sampleCount}.
func (m *P2PManager) GetAllLinkQualityStats() map[string]struct {
	NodeID      string
	SRTT        time.Duration
	RTO         time.Duration
	Jitter      time.Duration
	LossRate    float64
	SampleCount int
} {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := make(map[string]struct {
		NodeID      string
		SRTT        time.Duration
		RTO         time.Duration
		Jitter      time.Duration
		LossRate    float64
		SampleCount int
	})

	for _, p := range m.peers {
		if p.sendState != nil && p.ProxyName != "" {
			channelDrops := p.totalDataDrops() + atomic.LoadUint64(&p.controlChDrops)
			srtt, rto, jitter, lossRate, sampleCount := p.sendState.getStats(channelDrops)
			result[p.ProxyName] = struct {
				NodeID      string
				SRTT        time.Duration
				RTO         time.Duration
				Jitter      time.Duration
				LossRate    float64
				SampleCount int
			}{
				NodeID:      p.NodeID,
				SRTT:        srtt,
				RTO:         rto,
				Jitter:      jitter,
				LossRate:    lossRate,
				SampleCount: sampleCount,
			}
		}
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
		util.LogModuleInfo("p2p", "[P2P] incoming connection from %s (address=%s)", conn.RemoteAddr(), address)
		GlobalP2PManager.HandleP2PTransport(frame.NewStreamTransport(conn), address)
	} else {
		util.LogModuleDebug("p2p", "[P2P] no P2PManager initialized, closing conn from %s", conn.RemoteAddr())
		conn.Close()
	}
}

// HandleP2PTransport is called from server-side mesh channel handlers.
func HandleP2PTransport(t frame.FrameTransport, address string) {
	if GlobalP2PManager != nil {
		util.LogModuleInfo("p2p", "[P2P] incoming mesh transport from %s", address)
		GlobalP2PManager.HandleP2PTransport(t, address)
	} else {
		util.LogModuleDebug("p2p", "[P2P] no P2PManager initialized, dropping transport from %s", address)
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
