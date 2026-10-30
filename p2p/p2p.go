package p2p

import (
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
// The canonical definition lives in the mesh package (it travels inside
// GossipInfo); this alias keeps existing p2p callers unchanged.
const P2PProtocolVersion = mesh.P2PProtocolVersion

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
	frameType   byte
	data        []byte
	isControl   bool
	enqueueTime time.Time // set by enqueueWriteReq; used for sojourn measurement (Phase B)
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

	// Link identity (§5.3): local Seq assigned at peer creation; RemoteSeq
	// learned from the peer's first gossip. Link identity key = (NodeID, Seq).
	Seq          uint16 // 本端生成的序列号（本地递增）
	RemoteSeq    uint16 // 对端声明的序列号（首个 gossip 获得）
	FriendlyName string // 友好名称（代理名，仅展示）

	// Atomic fields for concurrent access (fix race condition #2 and #4)
	statusAtomic   atomic.Value // string: "connecting", "helloed", "upToDate", "failed"
	linkNICPtr     atomic.Pointer[mesh.LinkNIC]
	meshSenderPtr  atomic.Pointer[peerSender]
	lastSeenNano   atomic.Int64 // UnixNano timestamp of last seen (thread-safe)

	transport        frame.FrameTransport // guarded by transportMu: assignment races with stopPeer
	transportMu      sync.Mutex
	stopped          bool // set under transportMu by stopPeer; StartPeer re-checks after dial
	dnsQueue         chan writeReq // DNS packets (UDP port 53, highest priority)
	ackQueue         chan writeReq // TCP ACK packets (high priority)
	dataQueues       [256]chan writeReq // per-flow data queues (DRR fair scheduling, v2: 256 queues)
	drrOutput        chan writeReq // DRR scheduler output channel
	controlCh        chan writeReq // gossip/ping/pong priority queue
	stopCh           chan struct{}
	stopOnce         sync.Once // ensures stopCh is closed exactly once
	sessionDone      chan struct{} // closed when session fully ends (including LinkNIC cleanup)
	meshSender       *peerSender   // DEPRECATED: use meshSenderPtr
	proxy            *config.Proxy // proxy config this peer was started with

	// LinkNIC: 链路直接持有自己的 LinkNIC 指针（设计 §5.4）
	// DEPRECATED: use linkNICPtr
	linkNIC *mesh.LinkNIC

	// DRR scheduler for data queues
	drrScheduler *DRRScheduler

	// Per-peer random number generator (avoid global rand lock contention)
	rng   *rand.Rand
	rngMu sync.Mutex

	// LastError guard (issue 问题6: written by dial/write/heartbeat paths,
	// read by session logs and GetPeers snapshot — was unprotected)
	lastErrorMu sync.RWMutex

	// Ping/pong measurement state (v8)
	pingTracker *pingTracker // tracks ping/pong for RTT and loss rate

	// Channel overflow statistics (atomic counters)
	dnsQueueDrops       uint64 // Frames dropped due to dnsQueue full
	ackQueueDrops       uint64 // Frames dropped due to ackQueue full
	dataQueueDrops      [256]uint64 // Frames dropped due to dataQueues[i] full (v2: 256 queues)
	controlChDrops      uint64 // Frames dropped due to controlCh full

	// Egress health state (Phase B): locally-observed forwarding health.
	// Sampled into a window drop rate by sampleEgressWindow (called on the
	// mesh layer's 10s stats pull); sojourn is an EMA of enqueue→write delay.
	egressMu       sync.Mutex
	framesSent     uint64  // non-control frames written since last window sample
	lastDropCnt    uint64  // totalDataDrops() snapshot at last window sample
	dropRateEMA    float64 // EMA of window drop rate (0.0-1.0)
	sojournEMA     float64 // EMA of sojourn in ms
	sojournSamples uint64  // sojourn samples since last window sample
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

// LastError getter/setter (thread-safe)
func (p *Peer) GetLastError() string {
	p.lastErrorMu.RLock()
	defer p.lastErrorMu.RUnlock()
	return p.LastError
}

func (p *Peer) SetLastError(errMsg string) {
	p.lastErrorMu.Lock()
	p.LastError = errMsg
	p.lastErrorMu.Unlock()
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
		peer.SetStatus("stopped")
		close(peer.stopCh)
	})
	// Mark stopped and close the transport atomically: if a dial is still in
	// flight (transport not yet assigned), the flag ensures the assignment
	// site closes it instead — the stop signal can no longer be lost.
	peer.transportMu.Lock()
	peer.stopped = true
	t := peer.transport
	peer.transportMu.Unlock()
	if t != nil {
		t.Close()
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
// transport. Control frames (gossip/ping/pong) take priority over mesh
// data so they are not delayed behind bulk transfers. Exits on write error or stop.
// Data frames are coalesced into batches for efficient transport.
func (m *P2PManager) peerWriteLoop(peer *Peer) {
	const maxBatchFrames = 64 // Maximum frames per batch

	// writeSingleFrame writes a single frame.
	writeSingleFrame := func(req writeReq, isControl bool) bool {
		payload := req.data

		if req.frameType == frame.FrameMeshPacket {
			mesh.TraceForwarding("p2p_write_start", req.data, "peer=%s", peer.ID)
			// Diagnostic: log IPIP packets (proto=4) sent via writeSingleFrame
			if len(req.data) >= 20 && req.data[0]>>4 == 4 && req.data[9] == 4 {
				util.LogModuleDebug("p2p", "[P2P-IPIP-SEND-SINGLE] peer=%s len=%d", peer.ID, len(req.data))
			}
		}
		if err := peer.transport.Send(req.frameType, payload, isControl); err != nil {
			if req.frameType == frame.FrameMeshPacket {
				mesh.TraceForwarding("p2p_write_error", req.data, "peer=%s err=%v", peer.ID, err)
			}
			util.LogModuleWarn("p2p", "[P2P-DIAG] write error for peer.ID=%s proxy=%s nodeID=%s status=%s frameType=0x%02x: %v",
				peer.ID, peer.ProxyNameSafe(), peer.NodeID, peer.GetStatus(), req.frameType, err)
			peer.SetLastError(err.Error())
			peer.transport.Close()
			return false
		}
		if req.frameType == frame.FrameMeshPacket {
			mesh.TraceForwarding("p2p_write_ok", req.data, "peer=%s", peer.ID)
		}
		// Egress health (Phase B): a successfully written non-control frame counts
		// toward the window denominator; its sojourn feeds the EMA.
		if !req.isControl {
			peer.egressMu.Lock()
			peer.framesSent++
			peer.egressMu.Unlock()
			peer.recordSojourn(req)
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
		// Egress health (Phase B): batch frames are all data-path.
		peer.egressMu.Lock()
		peer.framesSent += uint64(len(batch))
		peer.egressMu.Unlock()
		for _, req := range batch {
			peer.recordSojourn(req)
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

	req.enqueueTime = time.Now()

	// Select channel based on priority:
	// 1. Control frames -> controlCh (highest priority)
	// 2. DNS packets (UDP port 53) -> dnsQueue (second highest)
	// 3. TCP ACK packets -> ackQueue (third highest)
	// 4. All other data -> dataQueues[flowHash] (DRR fair scheduling)
	// The DNS/ACK classifiers parse the payload as an IP header, so they
	// must only run on mesh data frames — JSON control payloads riding the
	// data queue (Phase C pings) would otherwise be misclassified.
	var ch chan writeReq
	isDNS := false
	isACK := false
	dataQueueIdx := -1

	isMeshPacket := req.frameType == frame.FrameMeshPacket
	if req.isControl {
		ch = peer.controlCh
	} else if isMeshPacket && isDNSPacket(req.data) {
		ch = peer.dnsQueue
		isDNS = true
	} else if isMeshPacket && isACKPacket(req.data) {
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
		Seq:            seq,
		drrScheduler:   newDRRScheduler(dataQueues),
		pingTracker:    newPingTracker(),
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

// StopAllPeers stops all active P2P peers but keeps the manager alive for restart.
// Used when mesh config changes and peers need to be re-established.
func (m *P2PManager) StopAllPeers() {
	m.mu.Lock()
	peers := make([]*Peer, 0, len(m.peers))
	for _, peer := range m.peers {
		peers = append(peers, peer)
	}
	m.proxyPeers = make(map[string]*Peer)
	m.peers = make(map[string]*Peer)
	m.mu.Unlock()

	util.LogModuleInfo("p2p", "[P2P] stopping all %d peers", len(peers))
	for _, peer := range peers {
		stopPeer(peer)
	}
	
	// Wait for all sessions to complete
	m.sessionsWG.Wait()
	util.LogModuleInfo("p2p", "[P2P] all peers stopped")
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

// StopPeerByNodeSeq disconnects the session to nodeID with the exact local Seq.
func (m *P2PManager) StopPeerByNodeSeq(nodeID string, seq uint16) {
	m.mu.Lock()
	peers := make([]*Peer, 0)
	for _, peer := range m.peers {
		if peer.NodeID == nodeID && peer.Seq == seq {
			peers = append(peers, peer)
		}
	}
	m.mu.Unlock()

	for _, peer := range peers {
		util.LogModuleInfo("p2p", "[P2P] stopping peer %s (node=%s seq=%d) due to connectivity failure", peer.ID, nodeID, seq)
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
	// Early check: if a peer already exists for this proxy, skip creation.
	// This prevents wasteful dial attempts when multiple goroutines call StartPeer
	// concurrently (e.g., config watcher + sessionLoop reconnect).
	m.mu.Lock()
	if existing, ok := m.proxyPeers[proxy.Name]; ok {
		m.mu.Unlock()
		util.LogModuleDebug("p2p", "[P2P] peer already exists for proxy %s (peer.ID=%s), skipping StartPeer", proxy.Name, existing.ID)
		return
	}
	m.mu.Unlock()

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
		pingTracker:  newPingTracker(),
	}
	peer.rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	peer.lastSeenNano.Store(time.Now().UnixNano())
	peer.SetStatus("connecting")

	// Register peer atomically. If another goroutine created a peer for this proxy
	// while we were dialing, abort and let the existing peer continue.
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		stopPeer(peer)
		return
	}
	if existing, ok := m.proxyPeers[proxy.Name]; ok {
		m.mu.Unlock()
		util.LogModuleInfo("p2p", "[P2P] peer created by another goroutine for proxy %s (peer.ID=%s), aborting duplicate", proxy.Name, existing.ID)
		stopPeer(peer)
		return
	}
	
	// Register new peer in both maps
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
			peer.SetLastError(err.Error())
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

		peer.transportMu.Lock()
		if peer.stopped {
			// stopPeer ran while the dial was in flight and had no transport
			// to close; honor the stop here instead of running a session
			// whose stop signal was already issued.
			peer.transportMu.Unlock()
			transport.Close()
			return
		}
		peer.transport = transport
		peer.transportMu.Unlock()
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
		peer.SetStatus("connecting")
		peer.SetLastError("") // dial succeeded — clear stale error (issue 问题6)
		m.notifyStatusChange()
		util.LogModuleInfo("p2p", "[P2P] connected to %s via proxy %s", proxy.Server, proxy.Name)

		// Run session in a closure so we can use defer for cleanup
		sessionStartTime := time.Now()
		func() {
			defer func() {
				sessionDuration := time.Since(sessionStartTime)
				// Log disconnection reason
				util.LogModuleInfo("p2p", "[P2P-DIAG] session ending for peer.ID=%s proxy=%s status=%s lastError=%v duration=%v",
					peer.ID, proxy.Name, peer.GetStatus(), peer.GetLastError(), sessionDuration)
				
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

		// Session ended. Clean up this peer from maps BEFORE spawning a fresh
		// StartPeer, so the new peer won't find the old one and won't need to
		// stop it. This ensures a clean handoff: old finishes → cleanup → new starts.
		m.mu.Lock()
		if current, ok := m.peers[peer.ID]; ok && current == peer {
			delete(m.peers, peer.ID)
		}
		if current, ok := m.proxyPeers[proxy.Name]; ok && current == peer {
			delete(m.proxyPeers, proxy.Name)
		}
		m.mu.Unlock()
		
		// Now spawn a fresh StartPeer so the new connection gets a brand-new
		// seq, peer.ID, and clean state.
		if peer.GetStatus() != "stopped" {
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
	
	// Send gossip immediately on connection establishment
	util.LogModuleInfo("p2p", "[P2P-DIAG] runSession calling broadcastGossipToPeer for peer.ID=%s proxy=%s", peer.ID, proxyName)
	m.broadcastGossipToPeer(peer)
	util.LogModuleInfo("p2p", "[P2P-DIAG] runSession broadcastGossipToPeer completed for peer.ID=%s", peer.ID)

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
					peer.SetLastError(fmt.Sprintf("heartbeat timeout: no data for %v", time.Since(lastSeenTime)))
					peer.transport.Close()
					return
				}
			}
		}
	}()
	defer close(timeoutDone)

	// Ping timer: send ping every 1 second for link quality measurement
	pingDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-pingDone:
				return
			case <-peer.stopCh:
				return
			case <-ticker.C:
				m.sendPing(peer)
			}
		}
	}()
	defer close(pingDone)

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
			peer.SetLastError(err.Error())
			frame.ReturnBuffer(originalPayload)
			return
		}
		util.LogModuleDebug("p2p", "[P2P] received frame type=0x%02x len=%d from %s", frameType, len(payload), peer.ID)

		switch frameType {
		case frame.FrameGossip:
			if len(payload) > 0 {
				m.handleGossip(peer, payload)
			}
		case frame.FrameLinkPing:
			if len(payload) > 0 {
				m.handlePing(peer, payload)
			}
		case frame.FrameLinkPong:
			if len(payload) > 0 {
				m.handlePong(peer, payload)
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

// hashString generates a simple hash from a string (for sequence number generation)
// handleGossip processes a gossip message from a peer.
// In v8 there is no hello frame: the first gossip on a connection establishes the
// peer (registration + version check + Seq/RemoteSeq learning, design §5.3/§8.1).
func (m *P2PManager) handleGossip(peer *Peer, payload []byte) {
	// Validate JSON by unmarshaling (issue 7: avoid remarshaling)
	var info mesh.GossipInfo
	if err := json.Unmarshal(payload, &info); err != nil {
		util.LogModuleDebug("p2p", "[P2P] invalid gossip from %s: %v", peer.ID, err)
		return
	}

	if peer.NodeID == "" {
		// First gossip on this connection = establishment (v8 replaces hello).
		// On version mismatch or missing nodeID this closes the transport; the
		// sender check below then skips forwarding.
		m.establishPeerFromGossip(peer, info)
	} else {
		// Mandatory version check on every gossip (§8.1). Zero means a legacy
		// sender that predates version negotiation — treat as mismatch.
		if info.ProtocolVersion != P2PProtocolVersion {
			util.LogModuleWarn("p2p", "[P2P] protocol version mismatch from %s (node=%s): peer=%d local=%d, disconnecting",
				peer.ID, peer.NodeID, info.ProtocolVersion, P2PProtocolVersion)
			peer.transport.Close()
			return
		}
		// NodeID change: re-register with the new identity (design §5.3).
		if nodeID := extractNodeIDFromGossip(info); nodeID != "" && nodeID != peer.NodeID {
			util.LogModuleInfo("p2p", "[P2P] peer %s nodeID changed: %s -> %s", peer.ID, peer.NodeID, nodeID)
			m.reregisterPeer(peer, nodeID, info)
		}
	}

	// Forward raw payload directly to avoid unmarshal→marshal cycle (issue 7)
	if sender := peer.GetMeshSender(); m.meshHandler != nil && sender != nil {
		m.meshHandler.HandleTopologyGossip(sender, payload)
	}
}

// establishPeerFromGossip performs the v8 connection establishment on the first
// gossip frame: version check, nodeID extraction, Seq/RemoteSeq learning, mesh
// registration, and LinkNIC creation. This replaces v7's handleHello (§5.3).
func (m *P2PManager) establishPeerFromGossip(peer *Peer, info mesh.GossipInfo) {
	// 1. Version check (mandatory, §8.1)
	if info.ProtocolVersion != P2PProtocolVersion {
		util.LogModuleWarn("p2p", "[P2P] protocol version mismatch from %s: peer=%d local=%d, disconnecting",
			peer.ID, info.ProtocolVersion, P2PProtocolVersion)
		peer.transport.Close()
		return
	}

	// 2. Extract nodeID from ClaimedSubnets[hop=0]
	nodeID := extractNodeIDFromGossip(info)
	if nodeID == "" {
		util.LogModuleWarn("p2p", "[P2P] first gossip from %s has no claimed subnet with hop=0, disconnecting", peer.ID)
		peer.transport.Close()
		return
	}

	peer.NodeID = nodeID
	peer.SetLastError("")
	peer.SetStatus("helloed")
	now := time.Now()
	peer.LastSeen = now
	peer.lastSeenNano.Store(now.UnixNano())

	if info.FriendlyName != "" {
		peer.FriendlyName = info.FriendlyName
	}

	// 3. Learn RemoteSeq from first gossip (§5.3); local link identity is
	// (NodeID, Seq) — Seq was assigned at peer creation, no negotiation needed.
	peer.RemoteSeq = info.Seq

	m.notifyStatusChange()
	util.LogModuleInfo("p2p", "[P2P] peer established via first gossip: %s nodeID=%s seq=%d remoteSeq=%d", peer.ID, nodeID, peer.Seq, peer.RemoteSeq)

	// 4-5. Register with mesh handler and create LinkNIC (design §5.6)
	m.registerPeerWithMesh(peer, nodeID, info)

	peer.SetStatus("upToDate")
	m.notifyStatusChange()
}

// reregisterPeer handles a nodeID change on an established connection (§5.3):
// unregister the old identity, register the new one, recreate the LinkNIC.
func (m *P2PManager) reregisterPeer(peer *Peer, nodeID string, info mesh.GossipInfo) {
	if m.meshHandler != nil {
		if sender := peer.GetMeshSender(); sender != nil {
			m.meshHandler.UnregisterPeer(sender)
		}
		if linkNIC := peer.GetLinkNIC(); linkNIC != nil {
			m.meshHandler.DeleteLinkNIC(linkNIC)
			peer.SetLinkNIC(nil)
		}
	}
	peer.NodeID = nodeID
	peer.SetLastError("")
	m.registerPeerWithMesh(peer, nodeID, info)
	m.notifyStatusChange()
}

// registerPeerWithMesh registers a peer with the mesh handler and creates its
// LinkNIC. Shared by establishPeerFromGossip and reregisterPeer.
func (m *P2PManager) registerPeerWithMesh(peer *Peer, nodeID string, info mesh.GossipInfo) {
	if m.meshHandler == nil {
		return
	}
	ps := &peerSender{peer: peer, nodeID: nodeID}
	peer.SetMeshSender(ps)
	m.meshHandler.RegisterPeer(ps)

	// Create LinkNIC for this link (design §5.6); subnet from ClaimedSubnets[hop=0]
	var subnet string
	for _, cs := range info.ClaimedSubnets {
		if cs.Hop == 0 {
			subnet = cs.Subnet
			break
		}
	}
	if subnet == "" {
		util.LogModuleWarn("p2p", "[P2P] no subnet in gossip from %s (nodeID=%s), cannot create LinkNIC", peer.ID, nodeID)
		return
	}
	linkNIC := m.meshHandler.CreateLinkNIC(ps, nodeID, subnet)
	peer.SetLinkNIC(linkNIC)
	if linkNIC == nil {
		util.LogModuleWarn("p2p", "[P2P] failed to create LinkNIC for peer %s (nodeID=%s)", peer.ID, nodeID)
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

// PingInfo is the payload for ping/pong frames.
type PingInfo struct {
	Timestamp int64  `json:"timestamp"` // Send time (UnixNano)
	Seq       uint32 `json:"seq"`       // Sequence number (for loss rate calculation)
}

// rttSampler maintains RTT statistics using exponential weighted moving average.
type rttSampler struct {
	mu         sync.Mutex
	srtt       time.Duration // Smoothed RTT
	rttVar     time.Duration // RTT variance
	sampleCount int
	lastSample  time.Duration
}

// add records a new RTT sample and updates statistics.
func (r *rttSampler) add(rtt time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	
	r.sampleCount++
	r.lastSample = rtt
	
	if r.srtt == 0 {
		// First sample
		r.srtt = rtt
		r.rttVar = rtt / 2
	} else {
		// Exponential weighted moving average (similar to TCP)
		// α = 1/8, β = 1/4
		diff := rtt - r.srtt
		if diff < 0 {
			diff = -diff
		}
		r.rttVar = (3*r.rttVar + diff) / 4
		r.srtt = (7*r.srtt + rtt) / 8
	}
}

// getStats returns current RTT statistics.
func (r *rttSampler) getStats() (srtt, rttVar time.Duration, sampleCount int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.srtt, r.rttVar, r.sampleCount
}

const (
	// pingLossWindowSize is the sliding window for loss rate: the last N
	// resolved ping outcomes (acked or lost), per design §4.3.
	pingLossWindowSize = 20
	// pingTimeout: pending pings older than this are resolved as losses.
	pingTimeout = 5 * time.Second
	// pingLateGrace: after a timeout loss verdict, the seq stays in grace
	// this long so a late pong can overturn the sample (Phase C, 30s).
	pingLateGrace = 30 * time.Second
	// pingFailDisconnectThreshold: 10 consecutive pings without any pong
	// triggers link teardown (design §4.4).
	pingFailDisconnectThreshold = 10
)

// pingSample is one resolved window slot carrying its seq, so a late pong
// can locate and overturn its earlier loss verdict (Phase C).
type pingSample struct {
	seq   uint32
	acked bool
}

// pingTracker tracks pending ping requests for RTT and loss rate measurement.
// Loss rate is a sliding window over the last pingLossWindowSize resolved
// outcomes (design §4.3): stale pending pings resolved as losses, pongs as
// acks, send failures roll back without entering the window.
//
// Phase C: pings travel the data queue, so a congested link delays pongs
// past pingTimeout. Timeout verdicts enter a lateGrace window instead of
// being final; a pong arriving within the grace overturns its sample
// (acked=true, consecutiveLost reset, full RTT recorded — the congestion
// signal stays in SRTT while lossRate only reflects real loss).
type pingTracker struct {
	mu              sync.Mutex
	pendingPings    map[uint32]time.Time // seq -> send time (in flight)
	lateGrace       map[uint32]time.Time // seq -> send time (loss verdict, awaiting late pong)
	nextSeq         uint32
	samples         []pingSample // sliding window of resolved outcomes
	consecutiveLost int          // consecutive resolved losses (reset on any pong)
	rttSampler      *rttSampler
}

// newPingTracker creates a new ping tracker.
func newPingTracker() *pingTracker {
	return &pingTracker{
		pendingPings: make(map[uint32]time.Time),
		lateGrace:    make(map[uint32]time.Time),
		nextSeq:      1,
		rttSampler:   &rttSampler{},
	}
}

func (t *pingTracker) addSampleLocked(acked bool, seq uint32) {
	t.samples = append(t.samples, pingSample{seq: seq, acked: acked})
	if len(t.samples) > pingLossWindowSize {
		t.samples = t.samples[len(t.samples)-pingLossWindowSize:]
	}
}

// recordPing ages out stale pending pings as losses, then registers a new
// pending ping and returns its sequence number.
func (t *pingTracker) recordPing() uint32 {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()

	// Resolve stale pending pings as losses (no pong within pingTimeout);
	// keep them in lateGrace so a congestion-delayed pong can still overturn.
	cutoff := now.Add(-pingTimeout)
	for s, sendTime := range t.pendingPings {
		if sendTime.Before(cutoff) {
			delete(t.pendingPings, s)
			t.lateGrace[s] = sendTime
			t.addSampleLocked(false, s)
			t.consecutiveLost++
		}
	}

	// Purge grace entries past the late window — verdict is final.
	graceCutoff := now.Add(-pingLateGrace)
	for s, sendTime := range t.lateGrace {
		if sendTime.Before(graceCutoff) {
			delete(t.lateGrace, s)
		}
	}

	seq := t.nextSeq
	t.nextSeq++
	t.pendingPings[seq] = now
	return seq
}

// cancelPing removes a pending ping without counting it (send failure —
// the frame never entered the wire, so it must not poison the loss window).
func (t *pingTracker) cancelPing(seq uint32) {
	t.mu.Lock()
	delete(t.pendingPings, seq)
	t.mu.Unlock()
}

// recordPong records a pong receive and calculates RTT. Three-level lookup:
// in-flight → normal ack; lateGrace → overturn the earlier loss verdict;
// otherwise unknown/duplicate seq, ignored.
func (t *pingTracker) recordPong(seq uint32, timestamp int64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if sendTime, ok := t.pendingPings[seq]; ok {
		delete(t.pendingPings, seq)
		t.addSampleLocked(true, seq)
		t.consecutiveLost = 0
		t.rttSampler.add(time.Since(sendTime))
		return
	}

	if sendTime, ok := t.lateGrace[seq]; ok {
		delete(t.lateGrace, seq)
		for i := range t.samples {
			if t.samples[i].seq == seq && !t.samples[i].acked {
				t.samples[i].acked = true
				break
			}
		}
		t.consecutiveLost = 0
		// Full RTT still enters the sampler: on a congested link the late
		// pong is precisely the queuing-delay signal we want SRTT to see.
		t.rttSampler.add(time.Since(sendTime))
	}
}

// consecutiveLostCount returns the number of consecutive pings resolved as
// losses without any pong in between (design §4.4 disconnect heuristic).
func (t *pingTracker) consecutiveLostCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.consecutiveLost
}

// getLossRate returns the current loss rate (0.0 - 1.0) over the sliding window.
func (t *pingTracker) getLossRate() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()

	if len(t.samples) == 0 {
		return 0.0
	}
	lost := 0
	for _, s := range t.samples {
		if !s.acked {
			lost++
		}
	}
	return float64(lost) / float64(len(t.samples))
}

// getStats returns all link quality statistics.
func (t *pingTracker) getStats() (srtt, rto, jitter time.Duration, lossRate float64, sampleCount int) {
	srtt, rttVar, sampleCount := t.rttSampler.getStats()
	lossRate = t.getLossRate()
	
	// RTO = SRTT + 4 * RTTVAR (similar to TCP)
	rto = srtt + 4*rttVar
	if rto < 100*time.Millisecond {
		rto = 100 * time.Millisecond // Minimum RTO
	}
	
	jitter = rttVar
	
	return srtt, rto, jitter, lossRate, sampleCount
}

// handlePing processes a ping request from a peer and sends a pong response.
func (m *P2PManager) handlePing(peer *Peer, payload []byte) {
	var ping PingInfo
	if err := json.Unmarshal(payload, &ping); err != nil {
		util.LogModuleDebug("p2p", "[P2P] invalid ping from %s: %v", peer.ID, err)
		return
	}
	
	// Immediately reply with pong, echoing the same timestamp and seq
	pong := PingInfo{
		Timestamp: ping.Timestamp,
		Seq:       ping.Seq,
	}
	data, _ := json.Marshal(pong)
	enqueueWrite(peer, frame.FrameLinkPong, data, true)
	
	util.LogModuleDebug("p2p", "[P2P] sent pong to %s: seq=%d", peer.ID, ping.Seq)
}

// handlePong processes a pong response from a peer and calculates RTT.
func (m *P2PManager) handlePong(peer *Peer, payload []byte) {
	var pong PingInfo
	if err := json.Unmarshal(payload, &pong); err != nil {
		util.LogModuleDebug("p2p", "[P2P] invalid pong from %s: %v", peer.ID, err)
		return
	}
	
	// Record the pong response
	if peer.pingTracker != nil {
		peer.pingTracker.recordPong(pong.Seq, pong.Timestamp)
	}
	
	util.LogModuleDebug("p2p", "[P2P] received pong from %s: seq=%d", peer.ID, pong.Seq)
}

// sendPing sends a ping request to a peer for link quality measurement.
// Send failures roll back the pending ping (not counted as loss, §4.3).
// 10 consecutive pings without any pong → Warn + close (design §4.4).
func (m *P2PManager) sendPing(peer *Peer) {
	if peer.pingTracker == nil {
		return
	}

	// Check consecutive-loss threshold BEFORE sending (ages out stale pings)
	if peer.pingTracker.consecutiveLostCount() >= pingFailDisconnectThreshold {
		util.LogModuleWarn("p2p", "[P2P] link dead: %d consecutive pings without pong for peer.ID=%s proxy=%s nodeID=%s, closing",
			pingFailDisconnectThreshold, peer.ID, peer.ProxyNameSafe(), peer.NodeID)
		peer.SetLastError(fmt.Sprintf("link dead: %d consecutive pings without pong", pingFailDisconnectThreshold))
		peer.transport.Close()
		return
	}

	seq := peer.pingTracker.recordPing()
	ping := PingInfo{
		Timestamp: time.Now().UnixNano(),
		Seq:       seq,
	}
	data, _ := json.Marshal(ping)
	// Phase C: ping rides the data queue so its SRTT reflects data-plane
	// queuing. Pong stays on the priority queue (round trip ≈ base RTT +
	// forward queuing). A RED/enqueue rejection rolls back via cancelPing.
	if err := enqueueWrite(peer, frame.FrameLinkPing, data, false); err != nil {
		// Send failure: frame never entered the wire, roll back without counting
		peer.pingTracker.cancelPing(seq)
		util.LogModuleDebug("p2p", "[P2P] ping enqueue failed for %s: %v", peer.ID, err)
		return
	}

	util.LogModuleDebug("p2p", "[P2P] sent ping to %s: seq=%d", peer.ID, seq)
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

// BroadcastGossipToAll broadcasts gossip to all connected peers (used after subnet conflict resolution).
func (m *P2PManager) BroadcastGossipToAll() {
	m.mu.Lock()
	peers := make([]*Peer, 0, len(m.peers))
	for _, p := range m.peers {
	if p.meshSenderPtr.Load() != nil {
		peers = append(peers, p)
	}
}
	m.mu.Unlock()

	for _, peer := range peers {
		m.broadcastGossipToPeer(peer)
	}
	util.LogModuleDebug("p2p", "[P2P] broadcast gossip to all peers (%d)", len(peers))
}

// broadcastGossipToPeer sends a gossip message to a specific peer.
// The local Seq is always included: the peer uses it to set RemoteSeq
// from our first gossip (design §5.3) and to match reverse links.
func (m *P2PManager) broadcastGossipToPeer(peer *Peer) {
	info := mesh.GossipInfo{
		Cmd:             "gossip",
		ProtocolVersion: P2PProtocolVersion,
		Seq:             peer.Seq,
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
	enqueueWrite(peer, frame.FrameGossip, data, true)
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
			LastError: p.GetLastError(),
			LastSeen:  time.Unix(0, lastSeenNano),
		})
	}
	return result
}

// recordSojourn feeds one enqueue→write sample into the sojourn EMA (Phase B).
// Control frames skip measurement — their priority queue wait is by design.
func (p *Peer) recordSojourn(req writeReq) {
	if req.enqueueTime.IsZero() || req.isControl {
		return
	}
	ms := float64(time.Since(req.enqueueTime).Microseconds()) / 1000.0
	p.egressMu.Lock()
	if p.sojournSamples == 0 {
		p.sojournEMA = ms
	} else {
		p.sojournEMA += 0.2 * (ms - p.sojournEMA)
	}
	p.sojournSamples++
	p.egressMu.Unlock()
}

// sampleEgressWindow closes the current drop-rate window and returns the
// smoothed egress health. Called on the mesh layer's periodic (10s) stats
// pull, so the window length equals the pull interval. When the window saw
// no traffic, both metrics decay toward zero so idle links don't carry a
// stale congestion penalty.
func (p *Peer) sampleEgressWindow() (dropRate, sojournMs float64) {
	p.egressMu.Lock()
	defer p.egressMu.Unlock()

	drops := p.totalDataDrops()
	windowDrops := drops - p.lastDropCnt
	windowSent := p.framesSent
	p.lastDropCnt = drops
	p.framesSent = 0

	total := windowDrops + windowSent
	var rate float64
	if total > 0 {
		rate = float64(windowDrops) / float64(total)
	}
	p.dropRateEMA = 0.5*p.dropRateEMA + 0.5*rate

	if p.sojournSamples == 0 {
		p.sojournEMA *= 0.5 // idle window: decay toward 0
	}
	p.sojournSamples = 0

	return p.dropRateEMA, p.sojournEMA
}

// GetLinkQualityStats returns ACK-based link quality stats for a peer by nodeID.
// totalDataDrops returns the sum of all data queue drops (dns + ack + data[256]).
func (p *Peer) totalDataDrops() uint64 {	var total uint64
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
		if p.NodeID == nodeID && p.pingTracker != nil {
			return p.pingTracker.getStats()
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


// GetLinkQualityStatsByProxy returns ping/pong-based link quality stats for a specific link (proxy).
// Returns srtt, rto, jitter, lossRate, sampleCount. Returns zeros if peer not found.
func (m *P2PManager) GetLinkQualityStatsByProxy(proxyName string) (srtt, rto, jitter time.Duration, lossRate float64, sampleCount int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, p := range m.peers {
		if p.ProxyName == proxyName && p.pingTracker != nil {
			return p.pingTracker.getStats()
		}
	}
	return 0, 0, 0, 0, 0
}

// GetLinkQualityStatsByNodeSeq returns ping/pong-based link quality stats for an exact link.
func (m *P2PManager) GetLinkQualityStatsByNodeSeq(nodeID string, seq uint16) (srtt, rto, jitter time.Duration, lossRate float64, sampleCount int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, p := range m.peers {
		if p.NodeID == nodeID && p.Seq == seq && p.pingTracker != nil {
			return p.pingTracker.getStats()
		}
	}
	return 0, 0, 0, 0, 0
}

// GetEgressStatsByNodeSeq returns locally-observed egress health for an exact
// link: window-smoothed queue drop rate and sojourn EMA (Phase B).
// ok=false when no such peer is connected.
func (m *P2PManager) GetEgressStatsByNodeSeq(nodeID string, seq uint16) (dropRate, sojournMs float64, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, p := range m.peers {
		if p.NodeID == nodeID && p.Seq == seq {
			dropRate, sojournMs = p.sampleEgressWindow()
			return dropRate, sojournMs, true
		}
	}
	return 0, 0, false
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
		if p.pingTracker != nil && p.ProxyName != "" {
			srtt, rto, jitter, lossRate, sampleCount := p.pingTracker.getStats()
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
