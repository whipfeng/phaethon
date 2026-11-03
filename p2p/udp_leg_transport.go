package p2p

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"phaethon/frame"
	"phaethon/util"
)

// maxLegDatagram guards against datagrams that exceed the maximum UDP payload
// (65507 = 65535 - 8 UDP header - 20 IP header). Mesh packets (<=1600) never
// come close; oversize frames fall back to the TCP leg.
const maxLegDatagram = 65507

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
	dialFn func() (net.PacketConn, error) // active side only

	fsmMu sync.Mutex // guards fsm (events arrive from recvLoop, writeLoop, readLoops, ticker)

	ctrlCh chan legRecvResult
	dataCh chan legRecvResult
	legCh  chan legRecvResult
	closed chan struct{}

	closeOnce sync.Once

	pcMu sync.Mutex
	pc   net.PacketConn // active side channel
	// dialing guards concurrent dial spawns; lastDialAt backs the R retry.
	dialing    bool
	lastDialAt time.Time

	epMu sync.Mutex
	ep   *udpLegEndpoint // passive side

	legDrops uint64 // atomic: inbound/outbound datagram drops
}

type legRecvResult struct {
	frameType byte
	payload   []byte
	err       error
}

// newUDPLegTransport wraps base with a UDP leg. dialFn is required on the
// active (dial) side and nil on the passive (listen) side.
func newUDPLegTransport(base frame.FrameTransport, active bool, dialFn func() (net.PacketConn, error)) *udpLegTransport {
	t := &udpLegTransport{
		base:   base,
		fsm:    newUDPLegFSM(active, true, udpLegTimings{}),
		active: active,
		dialFn: dialFn,
		ctrlCh: make(chan legRecvResult, 16),
		dataCh: make(chan legRecvResult, 64),
		legCh:  make(chan legRecvResult, 256),
		closed: make(chan struct{}),
	}
	go t.baseReadLoop()
	go t.tickLoop()
	return t
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
			return t.writeLeg(frameType, payload)
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
	for _, f := range frames {
		viaUDP := false
		switch f.Type {
		case frame.FrameLinkPing, frame.FrameLinkPong:
			viaUDP = t.routeProbeViaUDP()
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
	if sentData {
		t.fsmMu.Lock()
		t.fsm.OnDataSent(time.Now())
		t.fsmMu.Unlock()
	}
	return nil
}

// writeLeg fire-and-forgets one frame datagram. Errors are drops by contract
// and must never surface as session write errors (the TCP leg is the session).
func (t *udpLegTransport) writeLeg(frameType byte, payload []byte) error {
	dgram := frame.EncodeDatagram(frameType, payload)
	if len(dgram) > maxLegDatagram {
		// 超载帧回 TCP 腿，不丢数据
		return t.base.Send(frameType, payload, false)
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
		if pc == nil {
			return nil // dialing in flight; drop (datagram semantics)
		}
		dst := &net.UDPAddr{IP: net.IPv4zero, Port: port}
		if _, err := pc.WriteTo(dgram, dst); err != nil {
			atomic.AddUint64(&t.legDrops, 1)
			util.LogModuleDebug("p2p", "[UDP-LEG] write drop: %v", err)
		}
		return nil
	}
	t.epMu.Lock()
	ep := t.ep
	t.epMu.Unlock()
	if ep == nil {
		return nil // no port allocated yet; drop
	}
	if err := ep.send(dgram); err != nil {
		atomic.AddUint64(&t.legDrops, 1)
		util.LogModuleDebug("p2p", "[UDP-LEG] passive send drop: %v", err)
	}
	return nil
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
		ft, payload, err := frame.ParseDatagramFrame(buf[:n])
		if err != nil || !frame.IsLegFrame(ft) {
			atomic.AddUint64(&t.legDrops, 1)
			continue
		}
		// 主动端校验回包 src 端口 == 协商端口 P（设计 §3）
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
		t.fsmMu.Lock()
		t.fsm.OnUDPInbound(time.Now(), src)
		t.fsmMu.Unlock()
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

// epPump consumes relay deliveries on the passive side.
func (t *udpLegTransport) epPump(ep *udpLegEndpoint) {
	for {
		select {
		case <-ep.done:
			return
		case <-t.closed:
			return
		case in := <-ep.inboundCh:
			ft, payload, err := frame.ParseDatagramFrame(in.datagram)
			if err != nil || !frame.IsLegFrame(ft) {
				atomic.AddUint64(&t.legDrops, 1)
				continue
			}
			srcAddr, _ := net.ResolveUDPAddr("udp", in.srcAddr)
			t.fsmMu.Lock()
			t.fsm.OnUDPInbound(time.Now(), srcAddr)
			t.fsmMu.Unlock()
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
		}
	}
}

// maybeRedial drives the R-backoff redial while the FSM sits in Advertising
// with no channel (fallback 2). The FSM intentionally emits no dial action
// here — dial liveness is a transport concern (dialing flag, lastDialAt).
func (t *udpLegTransport) maybeRedial(now time.Time) {
	if !t.active || t.dialFn == nil {
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
		case legActStartAdvertise, legActStopAdvertise:
			// gossip 注入是查询式的（gossipFields），无需动作
		}
	}
}

func (t *udpLegTransport) spawnDial() {
	if !t.active || t.dialFn == nil {
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
		pc, err := t.dialFn()
		t.pcMu.Lock()
		t.dialing = false
		if err == nil {
			t.pc = pc
		}
		t.pcMu.Unlock()
		if err != nil {
			util.LogModuleInfo("p2p", "[UDP-LEG] DialPacket failed (will retry in backoff): %v", err)
			t.fsmMu.Lock()
			acts := t.fsm.OnChannelResult(time.Now(), false)
			t.fsmMu.Unlock()
			t.executeActions(acts)
			return
		}
		util.LogModuleInfo("p2p", "[UDP-LEG] channel established via %s", pc.LocalAddr())
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

func (t *udpLegTransport) allocatePort() {
	t.epMu.Lock()
	defer t.epMu.Unlock()
	if t.ep != nil {
		return
	}
	ep := globalUDPLegRegistry.allocate()
	if ep == nil {
		util.LogModuleWarn("p2p", "[UDP-LEG] no free leg port in range %d-%d", udpLegPortMin, udpLegPortMax)
		return
	}
	t.ep = ep
	t.fsmMu.Lock()
	t.fsm.SetLocalPort(ep.port)
	t.fsmMu.Unlock()
	util.LogModuleInfo("p2p", "[UDP-LEG] allocated leg port %d", ep.port)
	go t.epPump(ep)
}

func (t *udpLegTransport) unregisterPort() {
	t.epMu.Lock()
	ep := t.ep
	t.ep = nil
	t.epMu.Unlock()
	if ep != nil {
		globalUDPLegRegistry.release(ep.port)
	}
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

func (t *udpLegTransport) Close() error {
	t.closeOnce.Do(func() {
		close(t.closed)
		t.base.Close()
		t.closeChannel()
		t.unregisterPort()
	})
	return nil
}
