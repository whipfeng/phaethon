package p2p

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"phaethon/dialer"
	"phaethon/frame"
	"phaethon/util"
)

// legFragmentLimit is the largest UDP payload that fits a 1500-byte path
// (1500 - outer IP 20 - UDP 8). A datagram above it is fragmented by the outer
// IP layer. The overlay MTU (tun.OverlayMTU = 1400) keeps every normal data
// frame at <= 1423 bytes, so crossing this limit means an inner packet bypassed
// the MTU/MSS clamps — a bug indicator, not a carrier-selection rule. Such a
// frame is still sent on the leg (IP reassembly delivers it, with
// all-or-nothing loss); the counter exists to catch regressions.
const legFragmentLimit = 1500 - 20 - 8

// udpLegDialRetry is the active side's channel redial interval while stuck in
// Advertising (design fallback 2: 建不起来保持宣告，R 秒退避重试). Must match
// the FSM's default backoff.
const udpLegDialRetry = 60 * time.Second

// udpLegTransport overlays an optional UDP data leg on the permanent TCP leg
// (docs/plans/p2p_udp_transport_design.md). It wraps the base frame transport:
//   - Send/SendBatch route per frame by type and FSM state (gossip always TCP;
//     ping/pong UDP only once the leg is active-bound; mesh data UDP from
//     Negotiated on). UDP send failures are drops, never session errors.
//   - Recv merges TCP and UDP inbound with control-frame priority preserved.
//   - A 1s tick drives the FSM (K reflux timeout, R backoff retry).
//
// Active side owns a PacketConn from the proxy's DialPacket; passive side is
// fed by the socks5 UDP relay through the port registry.
type udpLegTransport struct {
	base   frame.FrameTransport
	fsm    *udpLegFSM
	active bool
	// peerIP is the active side's UDP destination: the peer address taken from
	// the proxy configuration (the same address the TCP control link dials).
	// The passive side learns the active peer's address from the first inbound
	// datagram instead.
	peerIP net.IP

	fsmMu sync.Mutex // guards fsm (events arrive from recvLoop, writeLoop, readLoops, ticker)

	ctrlCh chan legRecvResult
	dataCh chan legRecvResult
	legCh  chan legRecvResult
	closed chan struct{}

	closeOnce sync.Once

	pcMu sync.Mutex
	// pc is the direct UDP socket: an ephemeral socket on the active side and
	// a port from the configured UDP range on the passive side.
	pc net.PacketConn
	// dialing guards concurrent channel spawns; lastDialAt backs the R retry.
	dialing    bool
	lastDialAt time.Time

	// stateNotify fires on advertise-state transitions (enter/exit advertising)
	// so the mesh layer immediately re-broadcasts gossip through its single
	// build path (design §3 R2): udp/udpPort injection is query-style, and a
	// state change must reach the peer without waiting for the 15s ticker.
	stateNotifyMu sync.Mutex
	stateNotify   func()

	legDrops uint64 // atomic: inbound/outbound datagram drops
	// legOversize counts frames that exceed the 1500-byte path budget and will
	// be IP-fragmented. Should stay 0 while the 1400 overlay MTU holds.
	legOversize uint64 // atomic

	// legCrypto seals/opens every leg datagram when the SOCKS5 link has a
	// password. nil (or a disabled cipher) means plaintext, the legacy behavior.
	// The passive side installs it when the SOCKS5 relay binds the leg port;
	// the active side installs it from its proxy password at construction.
	legCrypto atomic.Pointer[util.HTunnelCrypto]

	// Liveness diagnostics: counters and last datagram timestamps. A leg that
	// keeps sending but sees no inbound traffic is a one-way black hole; log
	// it so stalls can be attributed without guesswork.
	legIn        uint64 // atomic
	legOut       uint64 // atomic
	lastInNano   int64  // atomic, UnixNano
	lastOutNano  int64  // atomic, UnixNano
	firstOutNano int64  // atomic, UnixNano; first datagram of the current leg episode
	silentWarned int32  // atomic
}

type legRecvResult struct {
	frameType byte
	payload   []byte
	err       error
}

// newUDPLegTransport wraps base with a direct UDP leg. The active (dial) side
// sends to peerIP:port; the passive (listen) side binds a port from the
// configured UDP range and replies to the source address of the peer's first
// datagram. key is the optional mesh-wide PSK; empty keeps datagrams plaintext.
func newUDPLegTransport(base frame.FrameTransport, active bool, key string, peerIP net.IP) *udpLegTransport {
	t := &udpLegTransport{
		base:   base,
		fsm:    newUDPLegFSM(active, true, udpLegTimings{}),
		active: active,
		peerIP: peerIP,
		ctrlCh: make(chan legRecvResult, 16),
		dataCh: make(chan legRecvResult, 64),
		legCh:  make(chan legRecvResult, 256),
		closed: make(chan struct{}),
	}
	t.setLegCrypto(key)
	go t.baseReadLoop()
	go t.tickLoop()
	return t
}

// setLegCrypto installs the datagram cipher derived from the mesh-wide P2P
// key. Domain separation keeps it distinct from the stream frame cipher and
// from h_tunnel's own crypto even when the same PSK is reused. Empty key
// yields a disabled cipher (identity passthrough, legacy plaintext).
func (t *udpLegTransport) setLegCrypto(key string) {
	if key == "" {
		t.legCrypto.Store(util.NewHTunnelCrypto(""))
		return
	}
	t.legCrypto.Store(util.NewHTunnelCrypto("phaethon-udp-leg-v1:" + key))
}

func (t *udpLegTransport) cryptoSeal(dgram []byte) []byte {
	if c := t.legCrypto.Load(); c != nil {
		return c.SealBody(dgram)
	}
	return dgram
}

// cryptoOpen decrypts one datagram. ok=false means authentication failed and
// the datagram must be dropped (it is not a valid leg frame from our peer).
func (t *udpLegTransport) cryptoOpen(dgram []byte) ([]byte, bool) {
	c := t.legCrypto.Load()
	if c == nil {
		return dgram, true
	}
	plain, err := c.OpenBody(dgram)
	if err != nil {
		return nil, false
	}
	return plain, true
}

func (t *udpLegTransport) String() string { return "udp-leg" }

// --- send path ---

func (t *udpLegTransport) Send(frameType byte, payload []byte, isControl bool) error {
	switch frameType {
	case frame.FrameGossip:
		// gossip 恒走 TCP（兼管协商），永不上 UDP 腿
		return t.base.Send(frameType, payload, isControl)
	case frame.FrameLinkPing, frame.FrameLinkPong:
		if t.routeProbeViaUDP() {
			err := t.writeLeg(frameType, payload)
			t.fsmMu.Lock()
			t.fsm.OnDataSent(time.Now())
			t.fsmMu.Unlock()
			return err
		}
	case frame.FrameMeshPacket:
		if t.routeDataViaUDP() {
			err := t.writeLeg(frameType, payload)
			t.fsmMu.Lock()
			t.fsm.OnDataSent(time.Now())
			t.fsmMu.Unlock()
			return err
		}
	}
	return t.base.Send(frameType, payload, isControl)
}

func (t *udpLegTransport) SendBatch(frames []frame.Frame) error {
	if len(frames) == 0 {
		return nil
	}
	// Partition by route. Batches are homogeneous in practice (all mesh data).
	var tcpBatch []frame.Frame
	sentData := false
	sentProbe := false
	for _, f := range frames {
		viaUDP := false
		switch f.Type {
		case frame.FrameLinkPing, frame.FrameLinkPong:
			viaUDP = t.routeProbeViaUDP()
			if viaUDP {
				sentProbe = true
			}
		case frame.FrameMeshPacket:
			if t.routeDataViaUDP() {
				viaUDP = true
				sentData = true
			}
		}
		if viaUDP {
			t.writeLeg(f.Type, f.Payload)
		} else {
			tcpBatch = append(tcpBatch, f)
		}
	}
	if len(tcpBatch) > 0 {
		if err := t.base.SendBatch(tcpBatch); err != nil {
			return err
		}
	}
	if sentData || sentProbe {
		t.fsmMu.Lock()
		t.fsm.OnDataSent(time.Now())
		t.fsmMu.Unlock()
	}
	return nil
}

// writeLeg fire-and-forgets one frame datagram. Errors are drops by contract
// and must never surface as session write errors (the TCP leg is the session).
func (t *udpLegTransport) writeLeg(frameType byte, payload []byte) error {
	dgram := t.cryptoSeal(frame.EncodeDatagram(frameType, payload))
	if len(dgram) > legFragmentLimit {
		// Bug indicator: the 1400-byte overlay MTU (tun.OverlayMTU) should cap
		// frames at ~1423B. Do not switch carriers by frame size — that splits
		// one ordered flow across UDP/TCP. Send it and let IP reassemble.
		n := atomic.AddUint64(&t.legOversize, 1)
		if n == 1 || n%1000 == 0 {
			util.LogModuleWarn("p2p", "[UDP-LEG] frame %dB exceeds %dB path budget (count=%d) — inner MTU invariant broken, IP will fragment",
				len(dgram), legFragmentLimit, n)
		}
	}
	if t.active {
		t.fsmMu.Lock()
		port, ok := t.fsm.SendPort()
		t.fsmMu.Unlock()
		if !ok {
			return t.base.Send(frameType, payload, false)
		}
		t.pcMu.Lock()
		pc := t.pc
		t.pcMu.Unlock()
		if pc == nil || t.peerIP == nil {
			return nil // channel not bound yet; drop (datagram semantics)
		}
		dst := &net.UDPAddr{IP: t.peerIP, Port: port}
		t.markFirstOut()
		if _, err := pc.WriteTo(dgram, dst); err != nil {
			atomic.AddUint64(&t.legDrops, 1)
			util.LogModuleDebug("p2p", "[UDP-LEG] write drop: %v", err)
		}
		atomic.AddUint64(&t.legOut, 1)
		atomic.StoreInt64(&t.lastOutNano, time.Now().UnixNano())
		return nil
	}
	// Passive side: reply to the source address of the peer's first datagram.
	t.fsmMu.Lock()
	addr := t.fsm.PeerReturnAddr()
	t.fsmMu.Unlock()
	if addr == nil {
		return nil // peer hasn't sent anything yet; drop
	}
	t.pcMu.Lock()
	pc := t.pc
	t.pcMu.Unlock()
	if pc == nil {
		return nil
	}
	t.markFirstOut()
	if _, err := pc.WriteTo(dgram, addr); err != nil {
		atomic.AddUint64(&t.legDrops, 1)
		util.LogModuleDebug("p2p", "[UDP-LEG] passive send drop: %v", err)
	}
	atomic.AddUint64(&t.legOut, 1)
	atomic.StoreInt64(&t.lastOutNano, time.Now().UnixNano())
	return nil
}

// markFirstOut records the first datagram of the current leg episode. The
// silence check needs a start reference before any inbound has arrived;
// without it a fresh leg looks "silent for 24h" and is force-fallen back
// before the peer can answer its first probe.
func (t *udpLegTransport) markFirstOut() {
	if atomic.LoadInt64(&t.firstOutNano) == 0 {
		atomic.CompareAndSwapInt64(&t.firstOutNano, 0, time.Now().UnixNano())
	}
}

func (t *udpLegTransport) routeDataViaUDP() bool {
	t.fsmMu.Lock()
	defer t.fsmMu.Unlock()
	return t.fsm.RouteDataViaUDP()
}

func (t *udpLegTransport) routeProbeViaUDP() bool {
	t.fsmMu.Lock()
	defer t.fsmMu.Unlock()
	return t.fsm.RouteProbeViaUDP()
}

// --- recv path ---

func (t *udpLegTransport) Recv() (byte, []byte, error) {
	select {
	case r := <-t.ctrlCh:
		return r.frameType, r.payload, r.err
	default:
	}
	select {
	case r := <-t.ctrlCh:
		return r.frameType, r.payload, r.err
	case r := <-t.dataCh:
		return r.frameType, r.payload, r.err
	case r := <-t.legCh:
		return r.frameType, r.payload, r.err
	case <-t.closed:
		return 0, nil, io.ErrClosedPipe
	}
}

// baseReadLoop forwards TCP frames, preserving control priority.
func (t *udpLegTransport) baseReadLoop() {
	for {
		ft, payload, err := t.base.Recv()
		if err != nil {
			select {
			case t.ctrlCh <- legRecvResult{err: err}:
			case <-t.closed:
			}
			return
		}
		ch := t.dataCh
		if ft != frame.FrameMeshPacket {
			ch = t.ctrlCh
		}
		select {
		case ch <- legRecvResult{frameType: ft, payload: payload}:
		case <-t.closed:
			return
		}
	}
}

// pcReadLoop pumps inbound datagrams from the active side's PacketConn.
func (t *udpLegTransport) pcReadLoop(pc net.PacketConn) {
	buf := make([]byte, 65535)
	for {
		n, src, err := pc.ReadFrom(buf)
		if err != nil {
			select {
			case <-t.closed:
			default:
				util.LogModuleDebug("p2p", "[UDP-LEG] channel read ended: %v", err)
			}
			return
		}
		plain, ok := t.cryptoOpen(buf[:n])
		if !ok {
			atomic.AddUint64(&t.legDrops, 1)
			continue
		}
		ft, payload, err := frame.ParseDatagramFrame(plain)
		if err != nil || !frame.IsLegFrame(ft) {
			atomic.AddUint64(&t.legDrops, 1)
			continue
		}
		// Active side: the reply must come from the negotiated peer port.
		// Passive side learns the peer's address from the datagram itself.
		if t.active {
			t.fsmMu.Lock()
			port, ok := t.fsm.SendPort()
			t.fsmMu.Unlock()
			var srcPort int
			if ua, ok2 := src.(*net.UDPAddr); ok2 {
				srcPort = ua.Port
			}
			if !ok || srcPort != port {
				atomic.AddUint64(&t.legDrops, 1)
				continue
			}
		}
		t.fsmMu.Lock()
		t.fsm.OnUDPInbound(time.Now(), src)
		t.fsmMu.Unlock()
		atomic.AddUint64(&t.legIn, 1)
		atomic.StoreInt64(&t.lastInNano, time.Now().UnixNano())
		pkt := make([]byte, len(payload))
		copy(pkt, payload)
		select {
		case t.legCh <- legRecvResult{frameType: ft, payload: pkt}:
		case <-t.closed:
			return
		default:
			atomic.AddUint64(&t.legDrops, 1)
		}
	}
}

// --- FSM event feeds and action execution ---

// onPeerGossip feeds the peer's udp/udpPort fields from received gossip.
func (t *udpLegTransport) onPeerGossip(peerUDP bool, peerPort int) {
	t.fsmMu.Lock()
	acts := t.fsm.OnGossip(time.Now(), peerUDP, peerPort)
	t.fsmMu.Unlock()
	t.executeActions(acts)
}

// tickLoop drives FSM timers (K reflux timeout, R backoff).
func (t *udpLegTransport) tickLoop() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-t.closed:
			return
		case now := <-ticker.C:
			t.fsmMu.Lock()
			acts := t.fsm.Tick(now)
			t.fsmMu.Unlock()
			t.executeActions(acts)
			t.maybeRedial(now)
			t.checkLegSilence(now)
		}
	}
}

// SetStateNotify registers the callback fired on advertise-state transitions
// (enter/exit advertising). Called by the p2p layer; the callback triggers an
// immediate mesh gossip broadcast via the single build path (design §3 R2).
func (t *udpLegTransport) SetStateNotify(fn func()) {
	t.stateNotifyMu.Lock()
	defer t.stateNotifyMu.Unlock()
	t.stateNotify = fn
}

// checkLegSilence warns when the leg is actively sending but has received no
// datagram for 5s. That is a one-way black hole (NAT/port change or broken
// return path), not congestion; log it once per episode and re-arm when
// inbound resumes.
func (t *udpLegTransport) checkLegSilence(now time.Time) {
	lastOut := atomic.LoadInt64(&t.lastOutNano)
	if lastOut == 0 || now.Sub(time.Unix(0, lastOut)) > 3*time.Second {
		return
	}
	lastIn := atomic.LoadInt64(&t.lastInNano)
	ref := lastIn
	if ref == 0 {
		// No inbound yet this episode: measure from the first outbound so a
		// fresh leg gets a full grace period instead of looking 24h silent.
		ref = atomic.LoadInt64(&t.firstOutNano)
	}
	if ref == 0 {
		return
	}
	inAge := now.Sub(time.Unix(0, ref))
	if inAge > 5*time.Second {
		if atomic.CompareAndSwapInt32(&t.silentWarned, 0, 1) {
			util.LogModuleError("p2p", "[UDP-LEG] outbound active but no inbound for %v (out=%d in=%d drops=%d) - one-way leg black hole?",
				inAge.Round(time.Millisecond), atomic.LoadUint64(&t.legOut), atomic.LoadUint64(&t.legIn), atomic.LoadUint64(&t.legDrops))
			// Act earlier than the 10s K reflux timeout: inner TCP RTO
			// backoff can already reset connections after ~10s of ACK loss.
			t.fsmMu.Lock()
			acts := t.fsm.ForceFallback(now)
			t.fsmMu.Unlock()
			t.executeActions(acts)
		}
		return
	}
	if inAge < 2*time.Second {
		atomic.StoreInt32(&t.silentWarned, 0)
	}
}

// maybeRedial drives the R-backoff redial while the FSM sits in Advertising
// with no channel (fallback 2). The FSM intentionally emits no dial action
// here — dial liveness is a transport concern (dialing flag, lastDialAt).
func (t *udpLegTransport) maybeRedial(now time.Time) {
	if !t.active {
		return
	}
	t.fsmMu.Lock()
	phase := t.fsm.Phase()
	t.fsmMu.Unlock()
	if phase != udpLegAdvertising {
		return
	}
	t.pcMu.Lock()
	if t.dialing || t.pc != nil {
		t.pcMu.Unlock()
		return
	}
	due := t.lastDialAt.IsZero() || now.Sub(t.lastDialAt) >= udpLegDialRetry
	t.pcMu.Unlock()
	if due {
		t.spawnDial()
	}
}

func (t *udpLegTransport) executeActions(acts []udpLegAction) {
	for _, a := range acts {
		switch a {
		case legActDialChannel:
			t.spawnDial()
		case legActCloseChannel:
			t.closeChannel()
		case legActAllocatePort:
			t.allocatePort()
		case legActUnregisterPort:
			t.unregisterPort()
		case legActStartAdvertise:
			t.resetSilenceWindow()
			t.notifyStateChange()
		case legActStopAdvertise:
			t.resetSilenceWindow()
			t.notifyStateChange()
		}
	}
}

// resetSilenceWindow clears per-episode liveness references so the next
// advertising episode starts with a fresh grace period.
func (t *udpLegTransport) resetSilenceWindow() {
	atomic.StoreInt64(&t.firstOutNano, 0)
	atomic.StoreInt64(&t.lastInNano, 0)
	atomic.StoreInt32(&t.silentWarned, 0)
}

// notifyStateChange asks the mesh layer to re-broadcast gossip immediately so
// udp/udpPort changes reach the peer without waiting for the 15s ticker.
func (t *udpLegTransport) notifyStateChange() {
	t.stateNotifyMu.Lock()
	fn := t.stateNotify
	t.stateNotifyMu.Unlock()
	if fn != nil {
		fn()
	}
}

// spawnDial binds the active side's local UDP socket. There is no proxy UDP
// associate any more: the control link already told us the peer address and
// the peer's advertised UDP port, so this is just a plain datagram socket.
func (t *udpLegTransport) spawnDial() {
	if !t.active {
		return
	}
	t.pcMu.Lock()
	if t.dialing || t.pc != nil {
		t.pcMu.Unlock()
		return
	}
	t.dialing = true
	t.lastDialAt = time.Now()
	t.pcMu.Unlock()

	go func() {
		pc, err := net.ListenPacket("udp4", "0.0.0.0:0")
		t.pcMu.Lock()
		t.dialing = false
		if err == nil {
			t.pc = pc
		}
		t.pcMu.Unlock()
		if err != nil {
			util.LogModuleInfo("p2p", "[UDP-LEG] local UDP bind failed (will retry in backoff): %v", err)
			t.fsmMu.Lock()
			acts := t.fsm.OnChannelResult(time.Now(), false)
			t.fsmMu.Unlock()
			t.executeActions(acts)
			return
		}
		util.LogModuleInfo("p2p", "[UDP-LEG] local UDP channel bound on %s -> peer %s", pc.LocalAddr(), t.peerIP)
		t.fsmMu.Lock()
		acts := t.fsm.OnChannelResult(time.Now(), true)
		t.fsmMu.Unlock()
		t.executeActions(acts)
		go t.pcReadLoop(pc)
	}()
}

func (t *udpLegTransport) closeChannel() {
	t.pcMu.Lock()
	pc := t.pc
	t.pc = nil
	t.pcMu.Unlock()
	if pc != nil {
		pc.Close()
	}
}

// allocatePort binds the passive side's real UDP socket. ListenUDP applies the
// configured UDP port range, the same range used by the SOCKS5 relay.
func (t *udpLegTransport) allocatePort() {
	if t.active {
		return
	}
	t.pcMu.Lock()
	if t.pc != nil {
		t.pcMu.Unlock()
		return
	}
	t.pcMu.Unlock()
	pc, err := dialer.ListenUDP()
	if err != nil {
		util.LogModuleWarn("p2p", "[UDP-LEG] bind passive UDP port failed: %v", err)
		return
	}
	port := 0
	if ua, ok := pc.LocalAddr().(*net.UDPAddr); ok {
		port = ua.Port
	}
	t.pcMu.Lock()
	t.pc = pc
	t.pcMu.Unlock()
	t.fsmMu.Lock()
	t.fsm.SetLocalPort(port)
	t.fsmMu.Unlock()
	util.LogModuleInfo("p2p", "[UDP-LEG] passive leg bound on UDP port %d", port)
	go t.pcReadLoop(pc)
}

// unregisterPort closes the passive socket so the port is released when the
// control link (or the leg episode) goes away.
func (t *udpLegTransport) unregisterPort() {
	t.closeChannel()
}

// --- introspection for gossip build + admin ---

// gossipFields returns this link's udp/udpPort gossip fields.
func (t *udpLegTransport) gossipFields() (bool, int) {
	t.fsmMu.Lock()
	defer t.fsmMu.Unlock()
	if !t.fsm.ShouldAdvertise() {
		return false, 0
	}
	port, _ := t.fsm.AdvertisePort()
	return true, port
}

// transportForm returns the admin badge value: "udp" while the leg carries
// traffic, "tcp" when it exists but has fallen back, "" when no leg.
func (t *udpLegTransport) transportForm() string {
	if t.routeDataViaUDP() || t.routeProbeViaUDP() {
		return "udp"
	}
	return "tcp"
}

func (t *udpLegTransport) LegDrops() uint64 { return atomic.LoadUint64(&t.legDrops) }

// LegPhase returns the FSM phase for admin diagnostics.
func (t *udpLegTransport) LegPhase() string {
	t.fsmMu.Lock()
	defer t.fsmMu.Unlock()
	return t.fsm.Phase().String()
}

// LegCounters returns (inbound, outbound, drops) datagram counters.
func (t *udpLegTransport) LegCounters() (uint64, uint64, uint64) {
	return atomic.LoadUint64(&t.legIn), atomic.LoadUint64(&t.legOut), atomic.LoadUint64(&t.legDrops)
}

// LegPort returns the negotiated peer port (active) or the bound local port
// (passive) for admin diagnostics.
func (t *udpLegTransport) LegPort() int {
	t.fsmMu.Lock()
	defer t.fsmMu.Unlock()
	if p, ok := t.fsm.SendPort(); ok {
		return p
	}
	if p, ok := t.fsm.AdvertisePort(); ok {
		return p
	}
	return 0
}

// LegOversize reports frames that crossed the 1500-byte path budget and are
// therefore IP-fragmented. Healthy legs keep this at 0.
func (t *udpLegTransport) LegOversize() uint64 { return atomic.LoadUint64(&t.legOversize) }

func (t *udpLegTransport) Close() error {
	t.closeOnce.Do(func() {
		close(t.closed)
		t.base.Close()
		t.closeChannel()
		t.unregisterPort()
	})
	return nil
}
