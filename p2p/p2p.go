package p2p

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net"
	"os"
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

// commitPattern matches the git describe commit info: -<digits>-g<hash>.
// 不锚定结尾，允许 "-dirty"、自定义后缀（如 "-relay-log"）跟在 hash 后面。
var commitPattern = regexp.MustCompile(`-(\d+)-g[0-9a-f]+`)

// P2PProtocolVersion is the current P2P protocol version.
// The canonical definition lives in the mesh package (it travels inside
// GossipInfo); this alias keeps existing p2p callers unchanged.
const P2PProtocolVersion = mesh.P2PProtocolVersion

// protocolCompatible reports whether a peer's advertised version range
// overlaps ours. remoteMin <= 0 means a legacy sender without the field.
func protocolCompatible(remoteVersion, remoteMin int) bool {
	if remoteVersion < mesh.P2PProtocolVersionMin {
		return false
	}
	if remoteMin > P2PProtocolVersion {
		return false
	}
	return true
}

// negotiatedProtocolVersion is the version both sides can speak.
func negotiatedProtocolVersion(remoteVersion int) int {
	if remoteVersion < P2PProtocolVersion {
		return remoteVersion
	}
	return P2PProtocolVersion
}

// meshInboundWorkers is the number of goroutines that consume the per-flow
// inbound queues and inject incoming mesh frames into gVisor's netstack.
//
// Rationale: with 1 worker we observed channelDrops=161 (queue overflow)
// under moderate load, and with N workers racing on ONE queue, same-flow
// frames were injected out of order (~50% rcv_ooopack on the receiving inner
// socket — the dribble root cause, §3.8). The pinned compromise: one queue
// per worker, frames hashed onto a queue by inner-flow key — per-flow order
// preserved (one goroutine per flow) with N-way concurrency across flows.
// gVisor's DeliverNetworkPacket is thread-safe, so concurrent injection of
// DIFFERENT flows from multiple workers stays safe.
//
// Tuning: monitor channelDropStats in /api/mesh. Typical range: 2-8.
const meshInboundWorkers = 4

const (
	// meshInboundByteBudget bounds total bytes queued for inbound mesh
	// workers. Data frames beyond this are dropped: mesh data is an overlay
	// and inner TCP retransmits, while a stalled read loop would also delay
	// pongs and look like a dead link.
	meshInboundByteBudget = 64 << 20 // 64MB across all inbound workers

	// defaultMaxInboundSessions bounds concurrent passive (inbound) P2P
	// sessions. Overridable via PHAETHON_MAX_P2P_SESSIONS.
	defaultMaxInboundSessions = 64
)

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
	dataDrops      uint64 // total data queue drops (dns + budget + data[256])
	controlChDrops uint64
}

// P2PManager manages P2P connections to peers.
type P2PManager struct {
	mu         sync.Mutex
	peers      map[string]*Peer // keyed by peer.ID (localSeq-based; active + passive)
	proxyPeers map[string]*Peer // keyed by proxy.Name (active peers only, for dedup)
	nodeId     string
	version    string
	buildTag   string
	platform   string
	arch       string
	cache      *BinaryCache

	meshEnabled bool
	meshNodeID  string
	meshVIP     string
	meshHandler MeshHandler

	// udpLegInbound: manager-level switch for passive-side UDP legs.
	// Passive peers carry no proxy config, so StartPeer sets this when any
	// local proxy enables p2p_udp; HandleP2PTransport consults it.
	udpLegInbound atomic.Bool

	// stripeIDCounter issues opaque lane nonces for StripeAttach (§5.2).
	stripeIDCounter atomic.Uint32

	// meshInboundChs is one queue per inbound worker; enqueue pins a frame to
	// a queue by its inner-flow key so one flow is always injected into the
	// netstack by the same goroutine, in arrival order (§3.8). A single shared
	// queue with N racing consumers reordered same-flow frames (~50% ooopack
	// on the receiving inner socket, dribble root cause).
	// cleanupCh serializes mesh/netstack teardown outside m.mu. See D9:
	// UnregisterPeer may block on mesh channels and DeleteLinkNIC enters the
	// netstack, so neither may run while the manager lock is held.
	cleanupCh   chan peerCleanup
	cleanupStop chan struct{}
	cleanupWG   sync.WaitGroup

	meshInboundChs    [meshInboundWorkers]chan meshInboundPacket
	meshInboundDNSCh  chan meshInboundPacket // dedicated queue for inbound DNS frames (avoids cross-node DNS SERVFAIL)
	meshInboundStopCh chan struct{}          // stop signal for meshInboundLoop
	stopOnce          sync.Once
	sessionsWG        sync.WaitGroup
	stopped           bool

	linkSeqCounter uint64 // atomic counter for link sequence numbers

	// Channel overflow statistics (atomic counters)
	meshInboundDrops uint64 // Frames dropped due to meshInboundCh full

	// meshInboundBytes is the total bytes currently queued for inbound workers.
	meshInboundBytes atomic.Int64
	// maxInboundSessions bounds concurrent passive sessions (D11).
	maxInboundSessions atomic.Int32

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

// ownedMeshHandler is the optional zero-copy extension of MeshHandler.
// Handlers that only implement MeshHandler keep using the copying path.
type ownedMeshHandler interface {
	HandleMeshFrameOwned(fromNodeID string, linkNIC *mesh.LinkNIC, frame []byte, release func())
}

// MeshHandler handles mesh packets and gossip from P2P peers.
type MeshHandler interface {
	HandleMeshFrame(fromNodeID string, linkNIC *mesh.LinkNIC, frame []byte)
	HandleTopologyGossip(sender mesh.PeerSender, info mesh.GossipInfo)
	RegisterPeer(sender mesh.PeerSender)
	UnregisterPeer(sender mesh.PeerSender)
	BuildGossipInfo() *mesh.GossipInfo
	// TriggerGossip enqueues an immediate gossip broadcast through the mesh's
	// single build path (design §3 R2) — used when a UDP leg's advertise
	// state changes.
	TriggerGossip()
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

// GossipFields returns this link's UDP leg negotiation fields (udp/udpPort)
// for mesh-layer gossip injection (design §3 R1: every gossip build path
// must carry them; absence reads as "not advertising" on the peer).
func (s *peerSender) GossipFields() (bool, int) {
	if s.peer.leg == nil {
		return false, 0
	}
	return s.peer.leg.gossipFields()
}

// LaneAckFields returns this link's LaneAck gating fields for gossip
// injection — same per-link contract as GossipFields (docs/plans/
// mesh-control-isolation-inflight-cap.md §3.2): declarations always, rx
// reports only when the peer declared WantTx, gauge only when WantGauge.
func (s *peerSender) LaneAckFields() (capable, wantTx, wantGauge bool, rx []mesh.GossipLaneRx, gauge int) {
	if s.peer.stripe == nil {
		return false, false, false, nil, 0
	}
	return s.peer.stripe.laneGossipFields()
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
	dns         bool      // set by enqueueWriteReq for dnsQueue frames
	enqueueTime time.Time // set by enqueueWriteReq; used for sojourn measurement (Phase B)
}

// Data-queue admission control (mesh-datapath-perf design §3.1):
// a per-peer byte budget bounds memory across all 256 data queues; a
// per-queue frame cap bounds head-of-line blocking behind one flow.
const (
	// Phaethon: a 1MB budget is smaller than the BDP of a 300ms path at
	// 5~10MB/s (1.5~3MB), so a latency step (external or relay queueing)
	// makes the relay drop data frames, which the inner TCP sees as loss
	// and answers with SACK/RTO storms. 4MB covers the step without
	// unbounded memory growth; drops still apply beyond that.
	dataQueueByteBudget = 4 << 20  // 4MB per peer
	dataQueueMaxDepth   = 512      // frames per queue
	drrQuantum          = 64 << 10 // 64KB per queue per round (~43×1500B frames)
)

// DRRScheduler implements Deficit Round Robin fair scheduling for multiple data queues.
// v3: real deficit-gated draining (design §3.2) + per-peer byte budget refund.
type DRRScheduler struct {
	// queuesPtr is nil until the session receives its first data frame (D11:
	// data queues are allocated lazily so idle sessions do not reserve ~8MB).
	queuesPtr atomic.Pointer[[256]chan writeReq]
	deficits  [256]int
	quantum   int           // bytes per queue per round
	round     int           // next queue to check
	salt      uint32        // random salt for Jenkins hash
	notifyCh  chan struct{} // signaled when a packet is enqueued
	budget    *atomic.Int64 // peer data-queue byte budget; refunded on dequeue
}

// newDRRScheduler creates a new DRR scheduler with the given queues and the
// peer's shared data-queue byte budget (nil disables refunding).
func newDRRScheduler(queues *[256]chan writeReq, budget *atomic.Int64) *DRRScheduler {
	s := &DRRScheduler{
		quantum:  drrQuantum,
		round:    0,
		salt:     rand.Uint32(),
		notifyCh: make(chan struct{}, 1), // buffered channel for notification
		budget:   budget,
	}
	if queues != nil {
		s.queuesPtr.Store(queues)
	}
	return s
}

// emptyQueues is returned before lazy allocation; nil channels are never
// ready in a select, so DRR treats them as empty.
var emptyQueues [256]chan writeReq

func (s *DRRScheduler) getQueues() *[256]chan writeReq {
	if p := s.queuesPtr.Load(); p != nil {
		return p
	}
	return &emptyQueues
}

func (s *DRRScheduler) setQueues(q *[256]chan writeReq) {
	s.queuesPtr.Store(q)
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
// Does ONE round through all 256 queues, draining each queue up to its
// deficit (real DRR gate — a busy flow gets ~quantum bytes per round, so a
// single flow now yields multi-frame batches instead of degenerating to 1).
// If no packets are found, blocks until a packet arrives or stopCh is closed.
// Next round starts from where this round stopped (fair across rounds).
func (s *DRRScheduler) NextBatch(maxBatch int, stopCh <-chan struct{}) []writeReq {
	batch := make([]writeReq, 0, maxBatch)

	// drainRound makes one pass over all queues starting at s.round,
	// draining each non-empty queue until its deficit is exhausted.
	// Returns the queue index where the next round should start.
	drainRound := func() int {
		// Read the queue pointer per round: data queues are allocated lazily
		// on the first data frame (D11), so a pointer captured at entry would
		// pin the empty placeholder and never see the real queues.
		queues := s.getQueues()
		last := s.round
		for i := 0; i < 256 && len(batch) < maxBatch; i++ {
			idx := (s.round + i) % 256
			last = idx

			// Replenish deficit at the round boundary. A negative remnant
			// (over-drawn by at most one frame in the previous round)
			// carries over and reduces this round's effective quantum —
			// textbook DRR behavior.
			if s.deficits[idx] <= 0 {
				s.deficits[idx] += s.quantum
			}

		drainQueue:
			for len(batch) < maxBatch {
				select {
				case req := <-queues[idx]:
					s.deficits[idx] -= len(req.data)
					if s.budget != nil {
						s.budget.Add(int64(len(req.data)))
					}
					batch = append(batch, req)
				default:
					// Queue empty: reset so idle flows don't bank deficit.
					s.deficits[idx] = 0
					break drainQueue
				}
				if s.deficits[idx] <= 0 {
					break
				}
			}
		}
		return (last + 1) % 256
	}

	s.round = drainRound()
	if len(batch) > 0 {
		return batch
	}

	// No packets found, block until a packet arrives or stopped
	for {
		select {
		case <-stopCh:
			return batch
		case <-s.notifyCh:
			s.round = drainRound()
			if len(batch) > 0 {
				return batch
			}
			// Stale notification or packet consumed elsewhere, wait again
		}
	}
}

// StripeInfo is one dynamic lane's observability snapshot (design §7):
// probe quality, current rendezvous weight and sent counters.
type StripeInfo struct {
	LaneID     uint32  `json:"laneId"`
	SRTTMs     float64 `json:"srttMs"`   // probe srtt EMA (0 = no sample yet)
	LossRate   float64 `json:"lossRate"` // lost/sent over the lane's lifetime
	Weight     float64 `json:"weight"`   // current rendezvous weight (linkCost form)
	FramesSent uint64  `json:"framesSent"`
	BytesSent  uint64  `json:"bytesSent"`
}

// Peer represents a connected P2P peer.
type Peer struct {
	ID        string       `json:"id"`                  // unique connection ID (localSeq-based)
	ProxyName string       `json:"proxyName"`           // proxy config name (active peers only, empty for passive)
	NodeID    string       `json:"nodeId"`              // mesh node ID, extracted from hello's ClaimedSubnets[hop=0]
	Status    string       `json:"status"`              // current peer status: connecting, helloed, upToDate, failed
	Transport string       `json:"transport,omitempty"` // udp/tcp(+stripe×N) data-leg form (empty = no leg)
	Stripes   []StripeInfo `json:"stripes,omitempty"`   // dynamic lane details (design §7)
	LastError string       `json:"lastError,omitempty"` // last error message (empty if no error)
	LastSeen  time.Time    `json:"lastSeen"`

	// Link identity (§5.3): local Seq assigned at peer creation; RemoteSeq
	// learned from the peer's first gossip. Link identity key = (NodeID, Seq).
	Seq          uint16 // 本端生成的序列号（本地递增）
	RemoteSeq    uint16 // 对端声明的序列号（首个 gossip 获得）
	FriendlyName string // 友好名称（代理名，仅展示）

	// Atomic fields for concurrent access (fix race condition #2 and #4)
	statusAtomic atomic.Value // string: "connecting", "helloed", "upToDate", "failed"
	// negotiatedVersion is min(local, remote) after the version check (D25).
	negotiatedVersion atomic.Int32
	// peerCaps is the remote capability set from hello/gossip (D25). Nil means
	// the peer predates capability advertisement — treated as "no restriction".
	peerCaps          atomic.Value // map[string]bool
	linkNICPtr        atomic.Pointer[mesh.LinkNIC]
	meshSenderPtr     atomic.Pointer[peerSender]
	lastSeenNano      atomic.Int64 // UnixNano timestamp of last seen (thread-safe)
	pingDeadHeldNano  atomic.Int64 // last "link dead held by inbound traffic" log, for rate limiting
	lastPongNano      atomic.Int64 // last successful pong; hard liveness ceiling

	transport       frame.FrameTransport // guarded by transportMu: assignment races with stopPeer
	transportMu     sync.Mutex
	stopped         bool               // set under transportMu by stopPeer; StartPeer re-checks after dial
	dnsQueue        chan writeReq      // DNS packets (UDP port 53, highest priority)
	dataQueues      [256]chan writeReq // per-flow data queues (allocated lazily on first data frame)
	dataQueuesMu    sync.Mutex
	dataQueuesReady atomic.Bool
	drrOutput       chan writeReq // DRR scheduler output channel
	controlCh       chan writeReq // gossip/ping/pong priority queue
	stopCh          chan struct{}
	stopOnce        sync.Once        // ensures stopCh is closed exactly once
	sessionDone     chan struct{}    // closed when session fully ends (including LinkNIC cleanup)
	proxy           *config.Proxy    // proxy config this peer was started with
	leg             *udpLegTransport // optional UDP data leg (nil when p2p_udp off)
	stripe          *stripeTransport // session data-leg multiplexer (always set on new peers)

	// flowNotify wakes peerWriteLoop to re-check its in-flight credit after
	// a LaneAck/cap adjustment (§3.4 write gate). Buffered 1, drop-on-full.
	flowNotify chan struct{}

	// DRR scheduler for data queues
	drrScheduler *DRRScheduler

	// Data-queue admission control (design §3.1): atomic byte budget shared
	// by all 256 data queues; reserved on enqueue, refunded on dequeue.
	dataBudget atomic.Int64

	// Per-peer random number generator (avoid global rand lock contention)
	rng   *rand.Rand
	rngMu sync.Mutex

	// LastError guard (issue 问题6: written by dial/write/heartbeat paths,
	// read by session logs and GetPeers snapshot — was unprotected)
	lastErrorMu sync.RWMutex

	// Ping/pong measurement state (v8)
	pingTracker *pingTracker // tracks ping/pong for RTT and loss rate

	// Channel overflow statistics (atomic counters)
	dnsQueueDrops   uint64      // Frames dropped due to dnsQueue full
	dataQueueDrops  [256]uint64 // Frames dropped due to dataQueues[i] full (v2: 256 queues)
	controlChDrops  uint64      // Frames dropped due to controlCh full
	dataBudgetDrops uint64      // Frames dropped due to per-peer data byte budget exhausted (design §3.1)

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

// ensureDataQueues allocates the 256 DRR data queues on first use (D11).
// Idle/passive sessions therefore reserve only the small control queues.
func (p *Peer) ensureDataQueues() *[256]chan writeReq {
	if p.dataQueuesReady.Load() {
		return &p.dataQueues
	}
	p.dataQueuesMu.Lock()
	defer p.dataQueuesMu.Unlock()
	if !p.dataQueuesReady.Load() {
		for i := range p.dataQueues {
			p.dataQueues[i] = make(chan writeReq, dataQueueMaxDepth)
		}
		if p.drrScheduler != nil {
			p.drrScheduler.setQueues(&p.dataQueues)
		}
		p.dataQueuesReady.Store(true)
	}
	return &p.dataQueues
}

// resetDataQueues drops the queues (reconnect) so they are re-allocated on
// the next data frame.
func (p *Peer) resetDataQueues() {
	p.dataQueuesMu.Lock()
	defer p.dataQueuesMu.Unlock()
	for i := range p.dataQueues {
		p.dataQueues[i] = nil
	}
	p.dataQueuesReady.Store(false)
	if p.drrScheduler != nil {
		p.drrScheduler.setQueues(nil)
		p.drrScheduler.deficits = [256]int{}
		p.drrScheduler.round = 0
	}
}

// localCapabilities is the capability set this build advertises in hello and
// gossip (D25). Optional features are enabled per peer from the intersection
// of both sides' sets.
var localCapabilities = []string{mesh.CapabilityStripe}

func (p *Peer) recordCapabilities(list []string) {
	if len(list) == 0 {
		return
	}
	set := make(map[string]bool, len(list))
	for _, c := range list {
		set[c] = true
	}
	p.peerCaps.Store(set)
}

// SupportsCapability reports whether the remote peer advertised name. Peers
// that never advertise (older builds) are treated as supporting it, matching
// the behaviour before capability negotiation existed.
func (p *Peer) SupportsCapability(name string) bool {
	v := p.peerCaps.Load()
	if v == nil {
		return true
	}
	return v.(map[string]bool)[name]
}

// CapabilityList returns the recorded remote capability names (empty when the
// peer predates capability advertisement).
func (p *Peer) CapabilityList() []string {
	v := p.peerCaps.Load()
	if v == nil {
		return nil
	}
	set := v.(map[string]bool)
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	return out
}

// GetNegotiatedVersion returns min(local, remote) established at handshake.
func (p *Peer) GetNegotiatedVersion() int {
	if v := p.negotiatedVersion.Load(); v > 0 {
		return int(v)
	}
	return P2PProtocolVersion
}

// ProxyNameSafe returns the proxy name, or empty string if proxy is nil (passive/inbound peer).
func (p *Peer) ProxyNameSafe() string {
	if p.proxy == nil {
		return ""
	}
	return p.proxy.Name
}

// udpRoutingActive reports whether the peer's UDP leg currently routes data
// (design §7). Called by the stripe controller once per tick; leg is set
// once before the session starts and read here under transportMu.
func (p *Peer) udpRoutingActive() bool {
	p.transportMu.Lock()
	leg := p.leg
	p.transportMu.Unlock()
	return leg != nil && leg.routeDataViaUDP()
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
		meshInboundDNSCh:  make(chan meshInboundPacket, 4096),
		meshInboundStopCh: make(chan struct{}),
		cleanupCh:         make(chan peerCleanup, 256),
		cleanupStop:       make(chan struct{}),
	}
	m.cleanupWG.Add(1)
	go m.cleanupLoop()
	m.maxInboundSessions.Store(defaultMaxInboundSessions)
	if v := os.Getenv("PHAETHON_MAX_P2P_SESSIONS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			m.maxInboundSessions.Store(int32(n))
		}
	}
	// One inbound queue per worker; frames are pinned to a queue by flow key
	// (§3.8) so each flow is processed serially by one goroutine.
	for i := range m.meshInboundChs {
		m.meshInboundChs[i] = make(chan meshInboundPacket, 4096)
		go m.meshInboundLoop(m.meshInboundChs[i])
	}
	// DNS frames get their own deep queue + workers so a data-frame burst
	// cannot starve cross-node DNS (5s forward timeout).
	for i := 0; i < 2; i++ {
		go m.meshInboundLoop(m.meshInboundDNSCh)
	}
	return m
}

// SetMaxInboundSessions updates the passive-session limit (D11).
func (m *P2PManager) SetMaxInboundSessions(n int) {
	if n <= 0 {
		return
	}
	m.maxInboundSessions.Store(int32(n))
}

// beginSession reserves a session slot. inbound sessions are additionally
// bounded by maxInboundSessions so a connection flood cannot exhaust memory.
func (m *P2PManager) beginSession(inbound bool) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return false
	}
	if inbound {
		limit := int(m.maxInboundSessions.Load())
		if limit > 0 {
			passive := 0
			for _, p := range m.peers {
				if p.proxy == nil {
					passive++
				}
			}
			if passive >= limit {
				util.LogModuleWarn("p2p", "[P2P] inbound session limit reached (%d), rejecting new session", limit)
				return false
			}
		}
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
		close(m.cleanupStop)
		m.cleanupWG.Wait()
	})
}

// peerCleanup carries mesh objects that must be torn down outside m.mu.
type peerCleanup struct {
	sender  *peerSender
	linkNIC *mesh.LinkNIC
}

func (m *P2PManager) cleanupLoop() {
	defer m.cleanupWG.Done()
	for {
		select {
		case <-m.cleanupStop:
			return
		case req := <-m.cleanupCh:
			m.runPeerCleanup(req)
		}
	}
}

func (m *P2PManager) runPeerCleanup(req peerCleanup) {
	h := m.meshHandler
	if h == nil {
		return
	}
	if req.sender != nil {
		h.UnregisterPeer(req.sender)
	}
	if req.linkNIC != nil {
		h.DeleteLinkNIC(req.linkNIC)
	}
}

// enqueuePeerCleanup hands mesh teardown to the cleanup goroutine. Must not be
// called while holding m.mu. Falls back to synchronous cleanup when the queue
// is full so a NIC is never leaked.
func (m *P2PManager) enqueuePeerCleanup(req peerCleanup) {
	if req.sender == nil && req.linkNIC == nil {
		return
	}
	select {
	case m.cleanupCh <- req:
	default:
		m.runPeerCleanup(req)
	}
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
			// D25 negotiation results, for admin visibility.
			"protocolVersion": peer.GetNegotiatedVersion(),
			"capabilities":    peer.CapabilityList(),
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

	// [P2P-QDEPTH] diagnostic: slow writes = carrier backpressure (downstream
	// of this process); deep queue with fast writes = backlog inside this process.
	var qdWinStart = time.Now()
	var qdBytes, qdWrites int64
	var qdWriteTotal, qdWriteMax time.Duration
	var qdMaxSojourn time.Duration
	qdRecord := func(batch []writeReq, d time.Duration) {
		qdWrites++
		qdWriteTotal += d
		if d > qdWriteMax {
			qdWriteMax = d
		}
		for _, req := range batch {
			qdBytes += int64(len(req.data))
			if !req.isControl && !req.enqueueTime.IsZero() {
				if s := time.Since(req.enqueueTime); s > qdMaxSojourn {
					qdMaxSojourn = s
				}
			}
		}
		win := time.Since(qdWinStart)
		if win < 5*time.Second {
			return
		}
		queued := dataQueueByteBudget - peer.dataBudget.Load()
		if queued >= 256<<10 || qdWriteMax >= time.Second || qdMaxSojourn >= time.Second {
			util.LogModuleInfo("p2p", "[P2P-QDEPTH] peer=%s proxy=%s queued=%dKB drrOut=%d ctrl=%d maxSojourn=%v writes=%d busy=%v maxWrite=%v rate=%.1fKB/s",
				peer.ID, peer.ProxyNameSafe(), queued>>10, len(peer.drrOutput), len(peer.controlCh),
				qdMaxSojourn.Round(time.Millisecond), qdWrites, qdWriteTotal.Round(time.Millisecond),
				qdWriteMax.Round(time.Millisecond), float64(qdBytes)/1024/win.Seconds())
		}
		qdWinStart = time.Now()
		qdBytes, qdWrites, qdWriteTotal, qdWriteMax, qdMaxSojourn = 0, 0, 0, 0, 0
	}

	// writeSingleFrame writes a single frame.
	writeSingleFrame := func(req writeReq, isControl bool) bool {
		payload := req.data
		wStart := time.Now()
		defer func() { qdRecord([]writeReq{req}, time.Since(wStart)) }()

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

	// writeBatch writes a batch (control → DNS → data) as one SendBatch.
	writeBatch := func(batch []writeReq) bool {
		if len(batch) == 0 {
			return true
		}
		if len(batch) == 1 {
			return writeSingleFrame(batch[0], batch[0].isControl)
		}

		frames := make([]frame.Frame, len(batch))
		for i, req := range batch {
			frames[i] = frame.Frame{Type: req.frameType, Payload: req.data, Control: req.isControl}
			if req.frameType == frame.FrameMeshPacket {
				mesh.TraceForwarding("p2p_write_start", req.data, "peer=%s batch=%d", peer.ID, len(batch))
			}
		}

		wStart := time.Now()
		err := peer.transport.SendBatch(frames)
		qdRecord(batch, time.Since(wStart))
		if err != nil {
			for _, req := range batch {
				if req.frameType == frame.FrameMeshPacket {
					mesh.TraceForwarding("p2p_write_error", req.data, "peer=%s err=%v", peer.ID, err)
				}
			}
			util.LogModuleWarn("p2p", "[P2P] batch write error for %s: %v", peer.ID, err)
			peer.transport.Close()
			return false
		}

		dataFrames := 0
		for _, req := range batch {
			if req.frameType == frame.FrameMeshPacket {
				mesh.TraceForwarding("p2p_write_ok", req.data, "peer=%s batch=%d", peer.ID, len(batch))
			}
			if !req.isControl {
				dataFrames++
			}
		}
		// Egress health (Phase B): only non-control frames count.
		peer.egressMu.Lock()
		peer.framesSent += uint64(dataFrames)
		peer.egressMu.Unlock()
		for _, req := range batch {
			if !req.isControl {
				peer.recordSojourn(req)
			}
		}
		return true
	}

	for {
		// §3.4 write gate: when the session's in-flight credit is exhausted,
		// stop dequeuing drrOutput — surplus data stays in the DRR queues
		// (fair scheduling, byte budget, drops → inner TCP backs off) while
		// control/DNS frames keep flowing. LaneAck/cap adjustments wake us
		// via flowNotify. Peers without LaneAck support (stripe nil, or no
		// ack yet) report infinite credit: gate stays open, pre-B2 behavior.
		credit := int64(math.MaxInt64)
		if peer.stripe != nil {
			credit = peer.stripe.writeCredit()
		}
		var first writeReq
		if credit > 0 {
			select {
			case <-peer.stopCh:
				return
			case <-peer.flowNotify:
				continue
			case first = <-peer.controlCh:
			case first = <-peer.dnsQueue:
			case req, ok := <-peer.drrOutput:
				if !ok {
					return
				}
				first = req
			}
		} else {
			select {
			case <-peer.stopCh:
				return
			case <-peer.flowNotify:
			case first = <-peer.controlCh:
			case first = <-peer.dnsQueue:
			}
		}
		if !writeBatch(collectWriteBatch(peer, first, maxBatchFrames, credit)) {
			return
		}
	}
}

// collectWriteBatch adds the frames already waiting on the peer queues to
// first, ordered control → DNS → data, up to max frames. Data frames are
// additionally bounded by maxDataBytes (the write gate's remaining credit)
// so one batch cannot overshoot the cap by more than one frame.
func collectWriteBatch(peer *Peer, first writeReq, max int, maxDataBytes int64) []writeReq {
	var ctrl, dns, data []writeReq
	var dataBytes int64
	switch {
	case first.isControl:
		ctrl = append(ctrl, first)
	case first.dns:
		dns = append(dns, first)
	default:
		data = append(data, first)
		dataBytes += int64(len(first.data))
	}
	n := 1
	drain := func(ch chan writeReq, dst []writeReq) []writeReq {
		for n < max {
			select {
			case req := <-ch:
				dst = append(dst, req)
				n++
			default:
				return dst
			}
		}
		return dst
	}
	ctrl = drain(peer.controlCh, ctrl)
	dns = drain(peer.dnsQueue, dns)
	for n < max && dataBytes < maxDataBytes {
		select {
		case req := <-peer.drrOutput:
			data = append(data, req)
			dataBytes += int64(len(req.data))
			n++
		default:
			goto dataDone
		}
	}
dataDone:
	batch := make([]writeReq, 0, n)
	batch = append(batch, ctrl...)
	batch = append(batch, dns...)
	return append(batch, data...)
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

// meshInboundLoop consumes mesh frames from the given queue and calls HandleMeshFrame.
// Decouples mesh processing from P2P read loop to prevent blocking.
func (m *P2PManager) meshInboundLoop(ch chan meshInboundPacket) {
	for {
		select {
		case <-m.meshInboundStopCh:
			return
		case pkt := <-ch:
			release := func() {
				m.meshInboundBytes.Add(-int64(len(pkt.frame)))
				frame.ReturnBuffer(pkt.frame)
			}
			h := m.meshHandler
			if h != nil {
				mesh.TraceForwarding("mesh_handler", pkt.frame, "fromNode=%s", pkt.fromNodeID)
				if oh, ok := h.(ownedMeshHandler); ok {
					// Zero-copy (D6): the handler releases the buffer exactly
					// once — immediately on drop, or when gVisor drops the
					// last reference to the injected packet.
					oh.HandleMeshFrameOwned(pkt.fromNodeID, pkt.linkNIC, pkt.frame, release)
					continue
				}
				h.HandleMeshFrame(pkt.fromNodeID, pkt.linkNIC, pkt.frame)
			}
			// Copying path: return the buffer after processing (issue 10).
			release()
		}
	}
}

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
		return mesh.ErrPeerStopped
	default:
	}

	req.enqueueTime = time.Now()

	// Select channel based on priority:
	// 1. Control frames -> controlCh (highest priority)
	// 2. DNS packets (UDP port 53) -> dnsQueue (second highest)
	// 3. All other data -> dataQueues[flowHash] (DRR fair scheduling); pure
	//    TCP ACKs ride here too: their sparse queue is served every DRR round.
	// The DNS classifier parses the payload as an IP header, so they
	// must only run on mesh data frames — JSON control payloads riding the
	// data queue (Phase C pings) would otherwise be misclassified.
	var ch chan writeReq
	isDNS := false
	dataQueueIdx := -1
	need := int64(0) // data-queue byte reservation (set in the data branch)

	isMeshPacket := req.frameType == frame.FrameMeshPacket
	if req.isControl {
		ch = peer.controlCh
	} else if isMeshPacket && isDNSPacket(req.data) {
		ch = peer.dnsQueue
		isDNS = true
		req.dns = true
	} else {
		// All data packets: use DRR fair scheduling
		dataQueueIdx = peer.drrScheduler.hashFlow(req.data)
		ch = peer.ensureDataQueues()[dataQueueIdx]
		// Byte-budget admission (design §3.1): reserve bytes before enqueue
		// so a stalled transport cannot pile unbounded memory in the data
		// queues. The reservation is refunded when the DRR scheduler
		// dequeues the frame.
		need = int64(len(req.data))
		for {
			cur := peer.dataBudget.Load()
			if cur < need {
				atomic.AddUint64(&peer.dataBudgetDrops, 1)
				util.LogModuleWarn("p2p", "[P2P] data byte budget exhausted for %s (need=%d cur=%d), dropping frame type=0x%02x",
					peer.ID, need, cur, req.frameType)
				return ErrQueueFull
			}
			if peer.dataBudget.CompareAndSwap(cur, cur-need) {
				break
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
		// Refund the byte reservation for data-queue frames.
		if dataQueueIdx >= 0 {
			peer.dataBudget.Add(need)
		}
		if req.frameType == frame.FrameMeshPacket {
			util.LogModuleWarn("p2p", "[P2P-ENQUEUE-DIAG] QUEUE FULL peer=%s dataLen=%d chCap=%d priority=%s",
				peer.ID, len(req.data), cap(ch), channelPriorityName(ch, peer))
			mesh.TraceForwarding("p2p_queue_drop", req.data, "peer=%s", peer.ID)
		}
		if req.isControl {
			atomic.AddUint64(&peer.controlChDrops, 1)
		} else if isDNS {
			atomic.AddUint64(&peer.dnsQueueDrops, 1)
		} else if dataQueueIdx >= 0 {
			atomic.AddUint64(&peer.dataQueueDrops[dataQueueIdx], 1)
		}
		util.LogModuleWarn("p2p", "[P2P] write queue full for %s, dropping frame type=0x%02x", peer.ID, req.frameType)
		return ErrQueueFull
	}
}

// isDNSPacket checks if a mesh packet contains UDP traffic to/from port 53
// (DNS); IPIP is classified by its inner packet.
func isDNSPacket(data []byte) bool {
	data = frame.InnerPacket(data)
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

// hashFlow computes a flow hash for fair scheduling using Jenkins one-at-a-time.
// Uses 5-tuple (srcIP, dstIP, protocol, srcPort, dstPort) for TCP/UDP,
// falls back to 3-tuple (srcIP, dstIP, protocol) for other protocols.
// v2: Jenkins hash with random salt for better distribution.
func (s *DRRScheduler) hashFlow(data []byte) int {
	return hashFlowWithSalt(s.salt, data)
}

// hashFlowWithSalt is the standalone flow hash shared by the DRR scheduler
// and the stripe transport (each with its own salt, design §六: the stripe
// wrapper carries a salt independent of DRR's).
func hashFlowWithSalt(salt uint32, data []byte) int {
	return int(flowHash32(salt, data) % 256)
}

// flowHash32 is the full 32-bit Jenkins one-at-a-time flow hash;
// hashFlowWithSalt masks it to 8 bits for DRR queue indexing, the stripe
// wrapper keeps the full width for rendezvous scoring (design §六). IPIP is
// hashed by its inner packet so tunneled flows spread like native ones.
func flowHash32(salt uint32, data []byte) uint32 {
	data = frame.InnerPacket(data)
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
	h := salt

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

	return h
}

// channelPriorityName returns the priority name for logging
func channelPriorityName(ch chan writeReq, peer *Peer) string {
	if ch == peer.controlCh {
		return "control"
	} else if ch == peer.dnsQueue {
		return "dns"
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
	// Peek the first frame. A StripeAttach (design §5.2) binds this connection
	// to an existing session as an extra data leg — it is not a new session
	// and must not consume a session slot. Anything else is a primary
	// connection whose first frame (v8: gossip) is re-delivered through the
	// session path below.
	ft, payload, err := t.Recv()
	if err != nil {
		t.Close()
		return
	}
	if ft == frame.FrameStripeAttach {
		nodeID, seq, stripeID, derr := frame.DecodeStripeAttach(payload)
		frame.ReturnBuffer(payload)
		if derr != nil {
			util.LogModuleWarn("p2p", "[STRIPE] malformed attach from %s: %v", address, derr)
			t.Close()
			return
		}
		if m.attachStripeLeg(nodeID, seq, stripeID, t) {
			return // ownership transferred to the session's stripe transport
		}
		util.LogModuleWarn("p2p", "[STRIPE] no session for attach nodeID=%s seq=%d from %s, closing", nodeID, seq, address)
		t.Close()
		return
	}
	t = newFirstFrameTransport(t, ft, payload)
	if !m.beginSession(true) {
		t.Close()
		return
	}
	defer m.sessionsWG.Done()

	seq := uint16(atomic.AddUint64(&m.linkSeqCounter, 1))

	// Data queues are allocated lazily on the first data frame (D11).

	peer := &Peer{
		ID:          fmt.Sprintf("%d", seq),
		LastSeen:    time.Now(),
		transport:   t,
		dnsQueue:    make(chan writeReq, 512),
		drrOutput:   make(chan writeReq, 512),
		controlCh:   make(chan writeReq, 512),
		flowNotify:  make(chan struct{}, 1),
		stopCh:      make(chan struct{}),
		Seq:         seq,
		pingTracker: newPingTracker(),
	}
	peer.drrScheduler = newDRRScheduler(nil, &peer.dataBudget)
	peer.dataBudget.Store(dataQueueByteBudget)
	peer.rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	peer.lastSeenNano.Store(time.Now().UnixNano())
	peer.lastPongNano.Store(time.Now().UnixNano())
	peer.SetStatus("connecting")

	// stripeTransport: session data-leg multiplexer (design §5). Sits below
	// the UDP leg so UDP-Active traffic bypasses lane splitting entirely.
	// Passive side: no dialLane → the dynamic controller never adds lanes.
	sendCtrl := func(ft byte, data []byte) error {
		return enqueueWrite(peer, ft, data, true)
	}
	wakeWriteLoop := func() {
		select {
		case peer.flowNotify <- struct{}{}:
		default:
		}
	}
	// Experimental generic pacer for stream carriers (env-gated, default off).
	// Kept for the record: enabling it broke the link (synchronous pacing held
	// the write loop); a viable pacer needs async shaping + control priority.
	var paced *frame.PacedTransport
	if os.Getenv("PHAETHON_PACE_STREAM") == "1" {
		if sc, ok := t.(frame.StreamCarrier); ok && sc.IsStreamCarrier() {
			paced = frame.NewPacedTransport(t, 0)
			t = paced
		}
	}
	stripe := newStripeTransport(t, stripeConfig{
		sendControl: sendCtrl,
		onCredit:    wakeWriteLoop,
		onRate: func(r float64) {
			if paced != nil {
				paced.SetRate(0.9 * r)
			}
		},
		name: "passive:" + peer.ID,
	})
	peer.stripe = stripe

	// Passive-side UDP leg: manager-level switch (no proxy config here).
	if m.udpLegInbound.Load() {
		wrapped := newUDPLegTransport(stripe, false, nil)
		peer.leg = wrapped
		peer.transport = wrapped
	} else {
		peer.transport = stripe
	}
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		stopPeer(peer)
		return
	}
	m.peers[peer.ID] = peer
	m.mu.Unlock()

	// UDP leg: advertise-state changes enqueue an immediate mesh gossip
	// broadcast through the single build path (design §3 R2).
	if wrapped, ok := peer.transport.(*udpLegTransport); ok {
		wrapped.SetStateNotify(func() {
			if m.meshHandler != nil {
				m.meshHandler.TriggerGossip()
			}
		})
	}
	defer func() {
		stopPeer(peer)

		// Detach mesh objects under the lock, then tear them down outside it
		// (D9): UnregisterPeer can block and DeleteLinkNIC enters the netstack.
		sender := peer.GetMeshSender()
		linkNIC := peer.GetLinkNIC()
		peer.SetMeshSender(nil)
		peer.SetLinkNIC(nil)
		m.mu.Lock()
		if current, ok := m.peers[peer.ID]; ok && current == peer {
			delete(m.peers, peer.ID)
		}
		m.mu.Unlock()

		m.enqueuePeerCleanup(peerCleanup{sender: sender, linkNIC: linkNIC})
		util.LogModuleInfo("p2p", "[P2P] session ended for %s", peer.ID)
	}()

	go m.drrLoop(peer)
	go m.peerWriteLoop(peer)
	m.runSession(peer)
}

// attachStripeLeg binds an inbound StripeAttach connection to the existing
// session identified by (nodeID, seq) — the session incarnation key learned
// from gossip (§5.2). Returns false when no established session matches (the
// caller closes the connection).
func (m *P2PManager) attachStripeLeg(nodeID string, seq uint16, stripeID uint32, t frame.FrameTransport) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.peers {
		if p.NodeID == nodeID && p.RemoteSeq == seq && p.stripe != nil {
			// D25: dynamic lanes are a v10 feature advertised via capability.
			// Rejecting here (rather than after attach) keeps older peers from
			// receiving lane traffic they cannot demultiplex.
			if p.GetNegotiatedVersion() < P2PProtocolVersion || !p.SupportsCapability(mesh.CapabilityStripe) {
				util.LogModuleWarn("p2p", "[STRIPE] peer.ID=%s negotiated version %d caps=%v: lane %d not supported, rejecting attach",
					p.ID, p.GetNegotiatedVersion(), p.CapabilityList(), stripeID)
				return false
			}
			util.LogModuleInfo("p2p", "[STRIPE] attaching leg stripeID=%d to peer.ID=%s (nodeID=%s seq=%d)",
				stripeID, p.ID, nodeID, seq)
			if !p.stripe.attachLane(stripeID, t) {
				return false
			}
			return true
		}
	}
	return false
}

// dialStripeLane dials one extra data lane to the peer's proxy and performs
// the StripeAttach handshake, returning the assigned stripeID and the
// established transport for the caller to attachLane (design §5.2). The
// lane's identity on the passive side is (localNodeID, peer.Seq) — the same
// (nodeID, seq) incarnation key gossip carries. Invoked by the stripe
// controller (tasks 2.x); failures are logged and the lane simply doesn't
// exist, never fatal to the session.
func (m *P2PManager) dialStripeLane(peer *Peer, p2pDialer dialer.P2PDialer) (uint32, frame.FrameTransport, error) {
	// D25: only dial extra lanes to peers that negotiated v10 and advertised
	// the stripe capability.
	if peer.GetNegotiatedVersion() < P2PProtocolVersion || !peer.SupportsCapability(mesh.CapabilityStripe) {
		return 0, nil, fmt.Errorf("stripe: peer %s does not support dynamic lanes (version=%d caps=%v)",
			peer.ID, peer.GetNegotiatedVersion(), peer.CapabilityList())
	}
	transport, err := p2pDialer.DialP2P()
	if err != nil {
		util.LogModuleInfo("p2p", "[STRIPE] lane dial via proxy %s failed: %v", peer.ProxyNameSafe(), err)
		return 0, nil, err
	}
	stripeID := m.stripeIDCounter.Add(1)
	payload, err := frame.EncodeStripeAttach(m.meshNodeID, peer.Seq, stripeID)
	if err != nil {
		util.LogModuleWarn("p2p", "[STRIPE] encode attach failed: %v", err)
		transport.Close()
		return 0, nil, err
	}
	if err := transport.Send(frame.FrameStripeAttach, payload, true); err != nil {
		util.LogModuleInfo("p2p", "[STRIPE] attach send failed: %v", err)
		transport.Close()
		return 0, nil, err
	}
	util.LogModuleInfo("p2p", "[STRIPE] lane dialed for peer.ID=%s stripeID=%d", peer.ID, stripeID)
	return stripeID, transport, nil
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

	if !m.beginSession(false) {
		util.LogModuleDebug("p2p", "[P2P] manager stopped; not starting peer %s", proxy.Name)
		return
	}
	defer m.sessionsWG.Done()

	util.LogModuleInfo("p2p", "[P2P] StartPeer called for proxy %s (type=%s, p2p=%v)", proxy.Name, proxy.Type, proxy.IsP2P())

	// Passive-side sessions carry no proxy config; arm the manager-level
	// inbound switch when any local proxy enables the UDP leg.
	if proxy.IsP2PUDP() {
		m.udpLegInbound.Store(true)
	}

	seq := uint16(atomic.AddUint64(&m.linkSeqCounter, 1))

	// Data queues are allocated lazily on the first data frame (D11).

	peer := &Peer{
		ID:          fmt.Sprintf("%d", seq),
		ProxyName:   proxy.Name,
		LastSeen:    time.Now(),
		stopCh:      make(chan struct{}),
		sessionDone: make(chan struct{}),
		proxy:       proxy,
		Seq:         seq,
		dnsQueue:    make(chan writeReq, 512),
		drrOutput:   make(chan writeReq, 512),
		controlCh:   make(chan writeReq, 512),
		flowNotify:  make(chan struct{}, 1),
		pingTracker: newPingTracker(),
	}
	peer.drrScheduler = newDRRScheduler(nil, &peer.dataBudget)
	peer.dataBudget.Store(dataQueueByteBudget)
	peer.rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	peer.lastSeenNano.Store(time.Now().UnixNano())
	peer.lastPongNano.Store(time.Now().UnixNano())
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
		// Mesh cleanup is normally handled by the inner session defer. Detach
		// any leftover LinkNIC outside the lock and hand it to the cleanup
		// goroutine (D9).
		linkNIC := peer.GetLinkNIC()
		peer.SetLinkNIC(nil)
		m.mu.Lock()
		// Remove from both maps (prevent race with RestartPeer)
		if current, ok := m.peers[peer.ID]; ok && current == peer {
			delete(m.peers, peer.ID)
		}
		if current, ok := m.proxyPeers[proxy.Name]; ok && current == peer {
			delete(m.proxyPeers, proxy.Name)
		}
		m.mu.Unlock()
		if linkNIC != nil {
			m.enqueuePeerCleanup(peerCleanup{linkNIC: linkNIC})
		}
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
		// stripeTransport: session data-leg multiplexer (design §5). Sits below
		// the UDP leg so UDP-Active traffic bypasses lane splitting entirely.
		// The controller (tasks 2.x) dials extra lanes via dialStripeLane and
		// defers to an Active UDP leg. Lane dialing needs a P2P frame dialer;
		// without one the wrapper still splits/fans-in but never adds lanes.
		// h_tunnel applies only when its actual carrier is an ordered stream
		// (WebSocket). The POST/GET long-poll pool — including a WS→long-poll
		// fallback — has no lane concept, so it stays unwrapped.
		base := transport
		var stripe *stripeTransport
		htPool := strings.EqualFold(proxy.Type, config.ProxyH_TUNNEL) && proxy.IsP2P() && proxy.V2
		streamCarrier := false
		if sc, ok := transport.(frame.StreamCarrier); ok && sc.IsStreamCarrier() {
			streamCarrier = true
		}
		if !htPool || streamCarrier {
			sendCtrl := func(ft byte, data []byte) error {
				return enqueueWrite(peer, ft, data, true)
			}
			wakeWriteLoop := func() {
				select {
				case peer.flowNotify <- struct{}{}:
				default:
				}
			}
			if pd, ok := d.(dialer.P2PDialer); ok {
				stripe = newStripeTransport(transport, stripeConfig{
					dialLane: func() (uint32, frame.FrameTransport, error) {
						return m.dialStripeLane(peer, pd)
					},
					udpActive:   peer.udpRoutingActive,
					sendControl: sendCtrl,
					onCredit:    wakeWriteLoop,
					name:        "active:" + peer.ID,
				})
			} else {
				stripe = newStripeTransport(transport, stripeConfig{
					udpActive:   peer.udpRoutingActive,
					sendControl: sendCtrl,
					onCredit:    wakeWriteLoop,
					name:        "active:" + peer.ID,
				})
			}
			base = stripe
			if streamCarrier {
				util.LogModuleInfo("p2p", "[STRIPE] stream carrier wrapped for lanes: proxy=%s", proxy.Name)
			}
		}
		peer.stripe = stripe
		peer.transport = base
		peer.transportMu.Unlock()

		// Active-side UDP leg wrap (design §7 Phase 1): socks5 proxies with
		// p2p_udp on, dialer supports packet dial. Wrapping here (not in the
		// dialer) avoids the dialer→p2p import cycle.
		if proxy.IsP2PUDP() {
			if ud, ok2 := d.(dialer.UDPDialer); ok2 {
				wrapped := newUDPLegTransport(base, true, ud.DialPacket)
				peer.leg = wrapped
				peer.transportMu.Lock()
				peer.transport = wrapped
				peer.transportMu.Unlock()
			} else {
				util.LogModuleInfo("p2p", "[P2P] proxy %s: p2p_udp on but dialer %T lacks DialPacket, UDP leg disabled", proxy.Name, d)
			}
		}
		peer.dnsQueue = make(chan writeReq, 512)
		peer.controlCh = make(chan writeReq, 512)
		peer.drrOutput = make(chan writeReq, 512)
		peer.rng = rand.New(rand.NewSource(time.Now().UnixNano()))
		// Re-arm lazy data-queue allocation for the new session.
		peer.resetDataQueues()
		// Fresh queues hold no bytes: reset the admission budget to full.
		peer.dataBudget.Store(dataQueueByteBudget)
		peer.SetStatus("connecting")
		peer.SetLastError("") // dial succeeded — clear stale error (issue 问题6)
		m.notifyStatusChange()

		// UDP leg: advertise-state changes enqueue an immediate mesh
		// gossip broadcast (single build path, design §3 R2).
		if wrapped, ok := peer.transport.(*udpLegTransport); ok {
			wrapped.SetStateNotify(func() {
				if m.meshHandler != nil {
					m.meshHandler.TriggerGossip()
				}
			})
		}
		util.LogModuleInfo("p2p", "[P2P] connected to %s via proxy %s", proxy.Server, proxy.Name)

		// Run session in a closure so we can use defer for cleanup
		sessionStartTime := time.Now()
		func() {
			defer func() {
				sessionDuration := time.Since(sessionStartTime)
				// Log disconnection reason
				util.LogModuleInfo("p2p", "[P2P-DIAG] session ending for peer.ID=%s proxy=%s status=%s lastError=%v duration=%v",
					peer.ID, proxy.Name, peer.GetStatus(), peer.GetLastError(), sessionDuration)

				// Close the leg wrapper when present: it owns the PacketConn
				// and (passive) the registered leg port — closing only the raw
				// transport would leak both plus the leg's goroutines.
				if peer.leg != nil {
					peer.leg.Close()
				} else {
					transport.Close()
				}

				// Update status to disconnected before cleanup
				if peer.GetStatus() != "stopped" {
					peer.SetStatus("disconnected")
					m.notifyStatusChange()
				}

				// Detach mesh objects, then tear them down outside m.mu (D9).
				sender := peer.GetMeshSender()
				linkNIC := peer.GetLinkNIC()
				peer.SetMeshSender(nil)
				peer.SetLinkNIC(nil)
				m.enqueuePeerCleanup(peerCleanup{sender: sender, linkNIC: linkNIC})
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
				// Zero-copy: pass payload directly to the mesh layer.
				// Consumer (meshInboundLoop) MUST call frame.ReturnBuffer when done (issue 10).
				// DNS frames go to the dedicated deep queue so data bursts cannot starve them.
				if isDNSPacket(payload) {
					// DNS keeps drop-on-full: UDP is order-tolerant and a stuck
					// consumer must not stall the session read loop.
					select {
					case m.meshInboundDNSCh <- meshInboundPacket{fromNodeID: peer.NodeID, linkNIC: linkNIC, frame: payload}:
						m.meshInboundBytes.Add(int64(len(payload)))
						mesh.TraceForwarding("p2p_inbound_enqueued", payload, "peer=%s node=%s", peer.ID, peer.NodeID)
						payloadReturned = true // Consumer will return it
					default:
						atomic.AddUint64(&m.meshInboundDrops, 1)
						mesh.TraceForwarding("p2p_inbound_drop", payload, "peer=%s node=%s", peer.ID, peer.NodeID)
						util.LogModuleDebug("p2p", "[P2P] meshInboundDNSCh full, dropping frame from %s", peer.NodeID)
					}
				} else {
					// Data frames: pinned per-flow to one worker queue (§3.8).
					// A full queue gets a bounded wait, then the frame is
					// dropped (§3.9 R5): blocking indefinitely would stall this
					// read loop — pongs included — until the session times out.
					meshInboundChs := m.meshInboundChs
					q := meshInboundChs[frame.FlowKey(payload)%uint32(meshInboundWorkers)]
					pkt := meshInboundPacket{fromNodeID: peer.NodeID, linkNIC: linkNIC, frame: payload}
					// Traced before handoff: the consumer owns the buffer after.
					mesh.TraceForwarding("p2p_inbound_enqueued", payload, "peer=%s node=%s", peer.ID, peer.NodeID)
					// Never block the session read loop on a full inbound queue:
					// pongs would be delayed and the peer would look dead. Mesh
					// data is an overlay, so the inner TCP retransmits.
					if m.meshInboundBytes.Add(int64(len(payload))) > meshInboundByteBudget {
						m.meshInboundBytes.Add(-int64(len(payload)))
						atomic.AddUint64(&m.meshInboundDrops, 1)
						util.LogModuleDebug("p2p", "[P2P] mesh inbound byte budget exhausted, dropping frame from %s", peer.NodeID)
					} else {
						select {
						case q <- pkt:
							payloadReturned = true
						case <-m.meshInboundStopCh:
							m.meshInboundBytes.Add(-int64(len(payload)))
							frame.ReturnBuffer(payload)
							return
						case <-peer.stopCh:
							m.meshInboundBytes.Add(-int64(len(payload)))
							frame.ReturnBuffer(payload)
							return
						default:
							m.meshInboundBytes.Add(-int64(len(payload)))
							atomic.AddUint64(&m.meshInboundDrops, 1)
							util.LogModuleDebug("p2p", "[P2P] mesh inbound queue full, dropping frame from %s", peer.NodeID)
						}
					}
				}
			} else {
				util.LogModuleWarn("p2p", "[P2P] FrameMeshPacket dropped: peer=%s node=%s meshHandler=%v payloadLen=%d", peer.ID, peer.NodeID, m.meshHandler != nil, len(payload))
			}
		case frame.FrameLaneAck:
			// Consumed by stripeTransport; old peers without stripes log once and ignore.
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

	// Capability advertisement (D25): empty means an older build that did not
	// advertise anything — SupportsCapability treats that as unrestricted.
	peer.recordCapabilities(info.Capabilities)

	if peer.NodeID == "" {
		// First gossip on this connection = establishment (v8 replaces hello).
		// On version mismatch or missing nodeID this closes the transport; the
		// sender check below then skips forwarding.
		m.establishPeerFromGossip(peer, info)
	} else {
		// Mandatory version check on every gossip (§8.1). Zero means a legacy
		// sender that predates version negotiation — treat as mismatch.
		if !protocolCompatible(info.ProtocolVersion, info.MinProtocolVersion) {
			util.LogModuleWarn("p2p", "[P2P] protocol version incompatible from %s (node=%s): peer=%d min=%d local=%d, disconnecting (will keep probing)",
				peer.ID, peer.NodeID, info.ProtocolVersion, info.MinProtocolVersion, P2PProtocolVersion)
			peer.transport.Close()
			return
		}
		peer.negotiatedVersion.Store(int32(negotiatedProtocolVersion(info.ProtocolVersion)))
		// NodeID change: re-register with the new identity (design §5.3).
		if nodeID := extractNodeIDFromGossip(info); nodeID != "" && nodeID != peer.NodeID {
			util.LogModuleInfo("p2p", "[P2P] peer %s nodeID changed: %s -> %s", peer.ID, peer.NodeID, nodeID)
			m.reregisterPeer(peer, nodeID, info)
		}
	}

	// Forward the already-parsed struct. The raw payload lives in the transport's
	// pooled recv buffer and is reused by the next Recv() — passing it across
	// goroutines caused "JSON decoder out of sync" panics. Parsing once here and
	// shipping the value also eliminates the duplicate unmarshals downstream.
	if sender := peer.GetMeshSender(); m.meshHandler != nil && sender != nil {
		m.meshHandler.HandleTopologyGossip(sender, info)
	}

	// UDP leg negotiation fields feed the per-link FSM (design §3).
	if peer.leg != nil {
		peer.leg.onPeerGossip(info.UDP, info.UDPPort)
	}

	// LaneAck gating fields (§3.2): gossip is the periodic fallback channel
	// for tx reports/gauge telemetry; want declarations arm our reporting.
	if peer.stripe != nil && (info.Lack || info.WantTx || info.WantGauge || len(info.LaneRx) > 0 || info.Gauge > 0) {
		rx := make([]laneAckEntry, 0, len(info.LaneRx))
		for _, e := range info.LaneRx {
			rx = append(rx, laneAckEntry{laneID: e.ID, rx: e.Rx})
		}
		peer.stripe.onPeerLaneGossip(info.WantTx, info.WantGauge, rx, info.Gauge)
	}
}

// establishPeerFromGossip performs the v8 connection establishment on the first
// gossip frame: version check, nodeID extraction, Seq/RemoteSeq learning, mesh
// registration, and LinkNIC creation. This replaces v7's handleHello (§5.3).
func (m *P2PManager) establishPeerFromGossip(peer *Peer, info mesh.GossipInfo) {
	// 1. Version check (range negotiation, D25). Out-of-range peers are
	// disconnected for this attempt only; StartPeer keeps reconnecting so an
	// upgrade on either side recovers without manual intervention.
	if !protocolCompatible(info.ProtocolVersion, info.MinProtocolVersion) {
		util.LogModuleWarn("p2p", "[P2P] protocol version incompatible from %s: peer=%d min=%d local=%d, disconnecting (will keep probing)",
			peer.ID, info.ProtocolVersion, info.MinProtocolVersion, P2PProtocolVersion)
		peer.transport.Close()
		return
	}
	peer.negotiatedVersion.Store(int32(negotiatedProtocolVersion(info.ProtocolVersion)))

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
	mu          sync.Mutex
	srtt        time.Duration // Smoothed RTT
	rttVar      time.Duration // RTT variance
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
	// pingDeadInboundQuiet: ping loss only kills the link if no inbound frame
	// arrived for this long; inbound traffic proves the peer and the inbound
	// direction alive while pongs sit behind a deep outbound backlog.
	pingDeadInboundQuiet = 10 * time.Second
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
	peer.lastPongNano.Store(time.Now().UnixNano())

	util.LogModuleDebug("p2p", "[P2P] received pong from %s: seq=%d", peer.ID, pong.Seq)
}

// sendPing sends a ping request to a peer for link quality measurement.
// Send failures roll back the pending ping (not counted as loss, §4.3).
// 10 consecutive pings without any pong and no inbound frame for 10s → Warn +
// close (design §4.4).
func (m *P2PManager) sendPing(peer *Peer) {
	if peer.pingTracker == nil {
		return
	}

	// Check consecutive-loss threshold BEFORE sending (ages out stale pings)
	if peer.pingTracker.consecutiveLostCount() >= pingFailDisconnectThreshold {
		now := time.Now()
		// Hard liveness ceiling: a peer that has not produced a single pong for
		// 60s is dead even if unrelated inbound frames keep lastSeen fresh (a
		// one-way/stale link would otherwise sit "held" forever and never
		// re-dial). 60s is well above the ping interval and any transient
		// backlog that motivated the inbound-quiet hold below.
		if lastPong := peer.lastPongNano.Load(); lastPong != 0 && now.Sub(time.Unix(0, lastPong)) >= 60*time.Second {
			util.LogModuleWarn("p2p", "[P2P] link dead: no pong for %v (inbound active), closing peer.ID=%s proxy=%s nodeID=%s",
				now.Sub(time.Unix(0, lastPong)).Round(time.Second), peer.ID, peer.ProxyNameSafe(), peer.NodeID)
			peer.SetLastError("link dead: no pong for 60s")
			peer.transport.Close()
			return
		}
		if quiet := now.Sub(time.Unix(0, peer.lastSeenNano.Load())); quiet < pingDeadInboundQuiet {
			if now.Sub(time.Unix(0, peer.pingDeadHeldNano.Load())) >= 10*time.Second {
				peer.pingDeadHeldNano.Store(now.UnixNano())
				util.LogModuleInfo("p2p", "[P2P] link dead held: %d+ consecutive pings without pong but last inbound frame %v ago, peer.ID=%s proxy=%s nodeID=%s",
					pingFailDisconnectThreshold, quiet.Round(time.Millisecond), peer.ID, peer.ProxyNameSafe(), peer.NodeID)
			}
		} else {
			util.LogModuleWarn("p2p", "[P2P] link dead: %d consecutive pings without pong for peer.ID=%s proxy=%s nodeID=%s, closing",
				pingFailDisconnectThreshold, peer.ID, peer.ProxyNameSafe(), peer.NodeID)
			peer.SetLastError(fmt.Sprintf("link dead: %d consecutive pings without pong", pingFailDisconnectThreshold))
			peer.transport.Close()
			return
		}
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

// broadcastGossipToPeer sends a gossip message to a specific peer.
// The local Seq is always included: the peer uses it to set RemoteSeq
// from our first gossip (design §5.3) and to match reverse links.
func (m *P2PManager) broadcastGossipToPeer(peer *Peer) {
	info := mesh.GossipInfo{
		Cmd:                "gossip",
		ProtocolVersion:    P2PProtocolVersion,
		MinProtocolVersion: mesh.P2PProtocolVersionMin,
		Capabilities:       localCapabilities,
		Seq:                peer.Seq,
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

	// 注意：udp/udpPort 协商字段不经此引导帧注入——构建职责唯一归属
	// mesh 层 gossip 构建器（设计 §3 R2）。此刻双方 FSM 均在 None，
	// 对端收到"无 udp 字段"无副作用；进入宣告态由 mesh 立即补发。

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
		form := "tcp"
		if p.leg != nil {
			form = p.leg.transportForm() // "udp" (leg routing) or "tcp" (backing off)
		}
		p.transportMu.Lock()
		st := p.stripe
		p.transportMu.Unlock()
		if st != nil {
			if n := st.laneCount(); n > 0 && form != "udp" {
				// Design §7 badge: tcp+stripe×N once dynamic lanes attach;
				// a routing UDP leg owns the data path and stays "udp".
				form = fmt.Sprintf("tcp+stripe×%d", n)
			}
			if infos := st.snapshotInfo(); len(infos) > 0 {
				result[len(result)-1].Stripes = infos
			}
		}
		result[len(result)-1].Transport = form
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
// totalDataDrops returns the sum of all data queue drops (dns + budget + data[256]).
func (p *Peer) totalDataDrops() uint64 {
	var total uint64
	total += atomic.LoadUint64(&p.dnsQueueDrops)
	total += atomic.LoadUint64(&p.dataBudgetDrops)
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

// GetLinkConsecutiveLostByNodeSeq returns the pingTracker consecutive-loss
// count for an exact link (0 when no such peer is connected).
func (m *P2PManager) GetLinkConsecutiveLostByNodeSeq(nodeID string, seq uint16) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, p := range m.peers {
		if p.NodeID == nodeID && p.Seq == seq && p.pingTracker != nil {
			return p.pingTracker.consecutiveLostCount()
		}
	}
	return 0
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

// ParseVersionForCheck 导出 parseVersion 供 main 等外部包做"版本号是否可解析"检查。
// 仅用于 selectWorkerBinary 的 self-unparseable 防御；比较版本仍走 CompareVersions。
func ParseVersionForCheck(v string) (tag []int, commits int) {
	return parseVersion(v)
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
	// Pattern: something-N-ghash where N is the commit count.
	// 去掉 match 起的所有内容（含可能的自定义后缀），保留前面的纯 tag。
	if match := commitPattern.FindStringSubmatchIndex(v); match != nil {
		if n, err := strconv.Atoi(v[match[2]:match[3]]); err == nil {
			commits = n
		}
		// match[0] 是 match 起点，match[1] 是 match 终点（不含）
		// match[2]:match[3] 是 capture group 1（digits）
		v = v[:match[0]]
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
