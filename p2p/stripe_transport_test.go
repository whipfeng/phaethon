package p2p

import (
	"bytes"
	"encoding/json"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"phaethon/frame"
)

// stripeTransport skeleton (tasks 1.2): one primary lane must behave
// byte-for-byte like the unwrapped base transport.

func TestStripeTransportSendPassThrough(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	tr := newStripeTransport(frame.NewStreamTransport(server), stripeConfig{})
	defer tr.Close()

	go func() {
		if err := tr.Send(frame.FrameGossip, []byte("hello"), true); err != nil {
			t.Errorf("Send: %v", err)
		}
	}()
	ft, payload, err := frame.ReadFrame(client)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if ft != frame.FrameGossip || !bytes.Equal(payload, []byte("hello")) {
		t.Fatalf("got (%#x,%q)", ft, payload)
	}
}

func TestStripeTransportRecvPassThrough(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	tr := newStripeTransport(frame.NewStreamTransport(server), stripeConfig{})
	defer tr.Close()

	go func() {
		if err := frame.WriteFrame(client, frame.FrameMeshPacket, []byte("pkt")); err != nil {
			t.Errorf("WriteFrame: %v", err)
		}
	}()
	ft, payload, err := tr.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if ft != frame.FrameMeshPacket || !bytes.Equal(payload, []byte("pkt")) {
		t.Fatalf("got (%#x,%q)", ft, payload)
	}
}

func TestStripeTransportSendBatchPassThrough(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	tr := newStripeTransport(frame.NewStreamTransport(server), stripeConfig{})
	defer tr.Close()

	frames := []frame.Frame{
		{Type: frame.FrameMeshPacket, Payload: []byte("a")},
		{Type: frame.FrameMeshPacket, Payload: []byte("bb")},
		{Type: frame.FrameLinkPing, Payload: []byte("ccc")},
	}
	go func() {
		if err := tr.SendBatch(frames); err != nil {
			t.Errorf("SendBatch: %v", err)
		}
	}()
	for i, want := range frames {
		ft, payload, err := frame.ReadFrame(client)
		if err != nil {
			t.Fatalf("frame %d ReadFrame: %v", i, err)
		}
		if ft != want.Type || !bytes.Equal(payload, want.Payload) {
			t.Fatalf("frame %d: got (%#x,%q) want (%#x,%q)", i, ft, payload, want.Type, want.Payload)
		}
	}
}

func TestStripeTransportControlPriorityPreserved(t *testing.T) {
	// Recv must prioritize control frames over queued data, same as the base
	// streamTransport (FrameTransport contract).
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	tr := newStripeTransport(frame.NewStreamTransport(server), stripeConfig{})
	defer tr.Close()

	go func() {
		_ = frame.WriteFrame(client, frame.FrameMeshPacket, []byte("data"))
		_ = frame.WriteFrame(client, frame.FrameGossip, []byte("ctrl"))
	}()

	// Give the base readLoop time to enqueue data first, then control; Recv
	// must still return the control frame.
	time.Sleep(100 * time.Millisecond)

	ft, payload, err := tr.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if ft != frame.FrameGossip || !bytes.Equal(payload, []byte("ctrl")) {
		t.Fatalf("control frame not prioritized: got (%#x,%q)", ft, payload)
	}
}

func TestStripeTransportCloseClosesLanes(t *testing.T) {
	_, server := net.Pipe()
	base := frame.NewStreamTransport(server)
	tr := newStripeTransport(base, stripeConfig{})

	laneConn1, lanePeer1 := net.Pipe()
	laneConn2, lanePeer2 := net.Pipe()
	defer lanePeer1.Close()
	defer lanePeer2.Close()
	tr.attachLane(1, frame.NewStreamTransport(laneConn1))
	tr.attachLane(2, frame.NewStreamTransport(laneConn2))

	if n := tr.laneCount(); n != 2 {
		t.Fatalf("laneCount = %d, want 2", n)
	}
	if form := tr.transportForm(); form != "tcp+stripe×2" {
		t.Fatalf("transportForm = %q, want tcp+stripe×2", form)
	}

	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Primary closed: Recv must fail.
	if _, _, err := tr.Recv(); err == nil {
		t.Fatal("Recv after Close must fail")
	}
	// Lanes closed: writing to a lane peer must fail (EOF/broken pipe).
	if err := frame.WriteFrame(lanePeer1, frame.FrameMeshPacket, []byte("x")); err == nil {
		t.Fatal("lane 1 not closed after wrapper Close")
	}
	if err := frame.WriteFrame(lanePeer2, frame.FrameMeshPacket, []byte("x")); err == nil {
		t.Fatal("lane 2 not closed after wrapper Close")
	}
}

func TestStripeTransportZeroLanesBadge(t *testing.T) {
	_, server := net.Pipe()
	defer server.Close()
	tr := newStripeTransport(frame.NewStreamTransport(server), stripeConfig{})
	defer tr.Close()

	if n := tr.laneCount(); n != 0 {
		t.Fatalf("laneCount = %d, want 0", n)
	}
	if form := tr.transportForm(); form != "tcp" {
		t.Fatalf("transportForm = %q, want tcp", form)
	}
}

func TestStripeTransportIdempotentClose(t *testing.T) {
	_, server := net.Pipe()
	tr := newStripeTransport(frame.NewStreamTransport(server), stripeConfig{})
	if err := tr.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// --- lane machinery (tasks 1.5/1.6) ---

// synthTCPFlow builds a minimal IPv4/TCP packet distinct per flowIndex
// (5-tuple differs by source port) — enough for hashFlow/isDNSPacket.
func synthTCPFlow(flowIndex int) []byte {
	pkt := make([]byte, 24)
	pkt[0] = 0x45 // IPv4, IHL=5
	pkt[9] = 6    // TCP
	pkt[12], pkt[13], pkt[14], pkt[15] = 10, 0, 0, 1
	pkt[16], pkt[17], pkt[18], pkt[19] = 10, 0, 0, 2
	pkt[20], pkt[21] = byte(flowIndex>>8), byte(flowIndex)
	pkt[22], pkt[23] = 0x30, 0x39 // dport 12345
	return pkt
}

// synthDNSPacket builds an IPv4/UDP packet to destination port 53.
func synthDNSPacket() []byte {
	pkt := make([]byte, 28)
	pkt[0] = 0x45
	pkt[9] = 17 // UDP
	pkt[12], pkt[13], pkt[14], pkt[15] = 10, 0, 0, 1
	pkt[16], pkt[17], pkt[18], pkt[19] = 10, 0, 0, 2
	pkt[20], pkt[21] = 0xC0, 0x00 // sport 49152
	pkt[22], pkt[23] = 0, 53      // dport 53
	return pkt
}

type pipeRead struct {
	idx  int
	ft   byte
	data []byte
}

// readFramesUntilClose reads frames from conn until it errors (peer
// closed), reporting each with index idx.
func readFramesUntilClose(idx int, conn net.Conn, out chan<- pipeRead) {
	for {
		ft, payload, err := frame.ReadFrame(conn)
		if err != nil {
			return
		}
		out <- pipeRead{idx: idx, ft: ft, data: payload}
	}
}

func TestStripeTransportLaneDispatch(t *testing.T) {
	// Endpoints: 0 = primary base, 1..2 = lanes.
	var conns [3]struct {
		peerEnd net.Conn // test writes/reads this end
	}
	transports := make([]net.Conn, 3)
	for i := range conns {
		peerEnd, wrapped := net.Pipe()
		conns[i].peerEnd = peerEnd
		transports[i] = wrapped
		defer peerEnd.Close()
	}
	base := frame.NewStreamTransport(transports[0])
	tr := newStripeTransport(base, stripeConfig{})
	defer tr.Close()
	tr.attachLane(1, frame.NewStreamTransport(transports[1]))
	tr.attachLane(2, frame.NewStreamTransport(transports[2]))
	defer transports[1].Close()
	defer transports[2].Close()

	const flows = 64
	var batch []frame.Frame
	for i := 0; i < flows; i++ {
		pkt := synthTCPFlow(i)
		batch = append(batch,
			frame.Frame{Type: frame.FrameMeshPacket, Payload: pkt},
			frame.Frame{Type: frame.FrameMeshPacket, Payload: append([]byte(nil), pkt...)},
		)
	}
	batch = append(batch,
		frame.Frame{Type: frame.FrameGossip, Payload: []byte("gossip")},
		frame.Frame{Type: frame.FrameMeshPacket, Payload: synthDNSPacket()},
	)

	got := make(chan pipeRead, len(batch)+8)
	// Rendezvous distribution is salt-random: per-pipe frame counts are not
	// predictable, so each reader runs until its pipe closes and the main
	// loop counts arrivals up to the full batch.
	go readFramesUntilClose(0, conns[0].peerEnd, got)
	go readFramesUntilClose(1, conns[1].peerEnd, got)
	go readFramesUntilClose(2, conns[2].peerEnd, got)

	if err := tr.SendBatch(batch); err != nil {
		t.Fatalf("SendBatch: %v", err)
	}

	// flowIndex → pipe idx (affinity assertion)
	flowPipe := make(map[int]int)
	seen := 0
	timeout := time.After(5 * time.Second)
	for seen < flows*2+2 {
		select {
		case r := <-got:
			seen++
			if r.ft == frame.FrameGossip || bytes.Equal(r.data, synthDNSPacket()) {
				if r.idx != 0 {
					t.Fatalf("control/DNS frame landed on pipe %d, want primary", r.idx)
				}
				continue
			}
			flow := int(r.data[20])<<8 | int(r.data[21])
			if prev, ok := flowPipe[flow]; ok && prev != r.idx {
				t.Fatalf("flow %d split across pipes %d and %d", flow, prev, r.idx)
			}
			flowPipe[flow] = r.idx
		case <-timeout:
			t.Fatalf("timed out: got %d of %d frames", seen, flows*2+2)
		}
	}
	laneFlows := 0
	for _, idx := range flowPipe {
		if idx != 0 {
			laneFlows++
		}
	}
	if laneFlows == 0 {
		t.Fatal("no flow was dispatched to a dynamic lane")
	}
}

func TestStripeTransportLaneFanIn(t *testing.T) {
	_, baseServer := net.Pipe()
	defer baseServer.Close()
	tr := newStripeTransport(frame.NewStreamTransport(baseServer), stripeConfig{})
	defer tr.Close()

	lanePeer, laneServer := net.Pipe()
	defer lanePeer.Close()
	tr.attachLane(1, frame.NewStreamTransport(laneServer))
	defer laneServer.Close()

	// Mesh frame on a lane surfaces through Recv.
	go func() {
		_ = frame.WriteFrame(lanePeer, frame.FrameMeshPacket, []byte("via-lane"))
	}()
	ft, payload, err := tr.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if ft != frame.FrameMeshPacket || !bytes.Equal(payload, []byte("via-lane")) {
		t.Fatalf("got (%#x,%q)", ft, payload)
	}

	// Link ping on a lane is consumed by the probe machinery (task 2.1):
	// it never surfaces through Recv, and a pong echoes back on the lane.
	go func() {
		_ = frame.WriteFrame(lanePeer, frame.FrameLinkPing, []byte(`{"timestamp":111,"seq":9}`))
	}()
	rft, rpayload, err := frame.ReadFrame(lanePeer)
	if err != nil {
		t.Fatalf("pong ReadFrame: %v", err)
	}
	if rft != frame.FrameLinkPong {
		t.Fatalf("lane ping not answered with pong: got 0x%02x", rft)
	}
	var pong PingInfo
	if err := json.Unmarshal(rpayload, &pong); err != nil {
		t.Fatalf("pong decode: %v", err)
	}
	if pong.Timestamp != 111 || pong.Seq != 9 {
		t.Fatalf("pong echo mismatch: %+v", pong)
	}
	// The ping must not leak into the session Recv channel.
	select {
	case r := <-tr.ctrlCh:
		t.Fatalf("lane ping leaked to session: 0x%02x", r.frameType)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestStripeTransportLaneControlDropped(t *testing.T) {
	_, baseServer := net.Pipe()
	defer baseServer.Close()
	tr := newStripeTransport(frame.NewStreamTransport(baseServer), stripeConfig{})
	defer tr.Close()

	lanePeer, laneServer := net.Pipe()
	defer lanePeer.Close()
	tr.attachLane(1, frame.NewStreamTransport(laneServer))
	defer laneServer.Close()

	// A gossip frame on a lane is a protocol error: dropped, not forwarded.
	// The connection stays usable for mesh data.
	go func() {
		_ = frame.WriteFrame(lanePeer, frame.FrameGossip, []byte("bad"))
		_ = frame.WriteFrame(lanePeer, frame.FrameMeshPacket, []byte("good"))
	}()
	ft, payload, err := tr.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if ft != frame.FrameMeshPacket || !bytes.Equal(payload, []byte("good")) {
		t.Fatalf("control frame was not dropped: got (%#x,%q)", ft, payload)
	}
}

func TestStripeTransportLaneDeathRemovesLane(t *testing.T) {
	basePeer, baseServer := net.Pipe()
	defer basePeer.Close()
	defer baseServer.Close()
	tr := newStripeTransport(frame.NewStreamTransport(baseServer), stripeConfig{})
	defer tr.Close()

	lanePeer, laneServer := net.Pipe()
	tr.attachLane(1, frame.NewStreamTransport(laneServer))
	defer laneServer.Close()

	// Kill the lane; the wrapper must drop it and keep serving the session.
	lanePeer.Close()
	deadline := time.Now().Add(2 * time.Second)
	for tr.laneCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("lane not removed after peer close, laneCount=%d", tr.laneCount())
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Traffic falls back to the primary lane.
	go func() {
		_ = tr.Send(frame.FrameMeshPacket, synthTCPFlow(1), false)
	}()
	ft, payload, err := frame.ReadFrame(basePeer)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if ft != frame.FrameMeshPacket || !bytes.Equal(payload, synthTCPFlow(1)) {
		t.Fatalf("fallback frame wrong: (%#x,%q)", ft, payload)
	}
}

// TestStripeAttachLaneEndToEnd exercises the full 1.3→1.4 wiring: active side
// dials a lane via stripeAttachLane (StripeAttach first frame), passive side
// peeks and binds it to its session via attachStripeLeg; lane traffic fans
// into the passive session.
func TestStripeAttachLaneEndToEnd(t *testing.T) {
	const localNodeID = "vm"
	const activeSeq = 7

	// Passive session peer with its own stripe wrapper (SB).
	sessPeerConn, sessServer := net.Pipe()
	defer sessPeerConn.Close()
	defer sessServer.Close()
	passiveStripe := newStripeTransport(frame.NewStreamTransport(sessServer), stripeConfig{})
	defer passiveStripe.Close()
	peer := &Peer{
		ID:        "1",
		NodeID:    localNodeID,
		RemoteSeq: activeSeq, // learned from the active side's gossip
		stripe:    passiveStripe,
	}
	mgr := &P2PManager{
		peers:      map[string]*Peer{peer.ID: peer},
		meshNodeID: localNodeID,
	}

	// Active side session peer with its own stripe wrapper (SA).
	activeBasePeer, activeBaseServer := net.Pipe()
	defer activeBasePeer.Close()
	defer activeBaseServer.Close()
	activeStripe := newStripeTransport(frame.NewStreamTransport(activeBaseServer), stripeConfig{})
	defer activeStripe.Close()
	activePeer := &Peer{
		ID:     "2",
		NodeID: "peer-node",
		Seq:    activeSeq,
		stripe: activeStripe,
	}

	// Fake dialer: DialP2P returns one end of a fresh pipe. The passive end's
	// streamTransport must exist BEFORE the attach Send — net.Pipe writes
	// block until the peer reads (in production the accept path creates it
	// immediately, see HandleP2PConnection).
	laneActive, lanePassive := net.Pipe()
	defer laneActive.Close()
	lanePassiveTr := frame.NewStreamTransport(lanePassive)
	fakeDialer := &fakeP2PDialer{tr: frame.NewStreamTransport(laneActive)}

	// Active side: dial + attach (task 1.3).
	stripeID, laneTr, err := mgr.dialStripeLane(activePeer, fakeDialer)
	if err != nil {
		t.Fatalf("dialStripeLane: %v", err)
	}
	activeStripe.attachLane(stripeID, laneTr)
	if activeStripe.laneCount() != 1 {
		t.Fatalf("active laneCount = %d, want 1", activeStripe.laneCount())
	}

	// Passive side: peek the first frame on its end (as HandleP2PTransport
	// does), then bind it to the session (task 1.4).
	ft, payload, err := lanePassiveTr.Recv()
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	if ft != frame.FrameStripeAttach {
		t.Fatalf("first frame = %#x, want StripeAttach", ft)
	}
	nodeID, seq, stripeID, err := frame.DecodeStripeAttach(payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if nodeID != localNodeID || seq != activeSeq || stripeID == 0 {
		t.Fatalf("attach identity wrong: (%q,%d,%d)", nodeID, seq, stripeID)
	}
	if !mgr.attachStripeLeg(nodeID, seq, stripeID, lanePassiveTr) {
		t.Fatal("attachStripeLeg rejected a valid attach")
	}
	if passiveStripe.laneCount() != 1 {
		t.Fatalf("passive laneCount = %d, want 1", passiveStripe.laneCount())
	}

	// Lane → passive session fan-in (task 1.6).
	go func() {
		_ = frame.WriteFrame(laneActive, frame.FrameMeshPacket, []byte("lane-to-session"))
	}()
	rft, rpayload, err := passiveStripe.Recv()
	if err != nil {
		t.Fatalf("session Recv: %v", err)
	}
	if rft != frame.FrameMeshPacket || !bytes.Equal(rpayload, []byte("lane-to-session")) {
		t.Fatalf("fan-in got (%#x,%q)", rft, rpayload)
	}

	// Miss case: unknown seq must be rejected (caller closes the conn).
	if mgr.attachStripeLeg(nodeID, 999, stripeID, lanePassiveTr) {
		t.Fatal("attachStripeLeg accepted an unknown seq")
	}
}

type fakeP2PDialer struct {
	tr frame.FrameTransport
}

func (f *fakeP2PDialer) DialP2P() (frame.FrameTransport, error) { return f.tr, nil }

// --- dynamic controller (tasks 2.x) ---

// stripeCtrlTestWindow shortens the controller tunables for testing and
// returns a restore func.
func stripeCtrlTestWindow() func() {
	saved := map[string]any{}
	vars := map[string]any{
		"stripeCtrlTick":           &stripeCtrlTick,
		"stripeProbeInterval":      &stripeProbeInterval,
		"stripeProbeTimeout":       &stripeProbeTimeout,
		"stripeGaugeDecayPerTick":  &stripeGaugeDecayPerTick,
		"stripeGaugeIdleDecay":     &stripeGaugeIdleDecay,
		"stripeGaugeHighWatermark": &stripeGaugeHighWatermark,
		"stripeAddSustain":         &stripeAddSustain,
		"stripeAddInterval":        &stripeAddInterval,
		"stripeClearSustain":       &stripeClearSustain,
		"stripeMaxLanes":           &stripeMaxLanes,
		"stripeBaselineWindow":     &stripeBaselineWindow,
	}
	for name, ptr := range vars {
		switch p := ptr.(type) {
		case *time.Duration:
			saved[name] = *p
		case *float64:
			saved[name] = *p
		case *int:
			saved[name] = *p
		}
	}
	stripeCtrlTick = 20 * time.Millisecond
	stripeProbeInterval = time.Hour // probes disabled in controller tests
	stripeProbeTimeout = time.Hour
	stripeGaugeDecayPerTick = 0.5
	stripeGaugeIdleDecay = 250 * time.Millisecond
	stripeGaugeHighWatermark = 0.7
	stripeAddSustain = 100 * time.Millisecond
	stripeAddInterval = 50 * time.Millisecond
	stripeClearSustain = 300 * time.Millisecond
	stripeMaxLanes = 4
	stripeBaselineWindow = time.Hour // no re-baseline escape mid-test
	return func() {
		for name, ptr := range vars {
			switch p := ptr.(type) {
			case *time.Duration:
				*p = saved[name].(time.Duration)
			case *float64:
				*p = saved[name].(float64)
			case *int:
				*p = saved[name].(int)
			}
		}
	}
}

// slowTransport adds a switchable delay to every base send so sojourn
// sampling sees congestion without a real network. Tests first run it at
// zero delay to establish the fast baseline (§5.4), then setDelay to
// simulate congestion relative to that baseline.
type slowTransport struct {
	base  frame.FrameTransport
	delay atomic.Int64 // nanoseconds
}

func (s *slowTransport) setDelay(d time.Duration) { s.delay.Store(int64(d)) }

func (s *slowTransport) sleep() { time.Sleep(time.Duration(s.delay.Load())) }

func (s *slowTransport) Send(ft byte, payload []byte, ctrl bool) error {
	s.sleep()
	return s.base.Send(ft, payload, ctrl)
}

func (s *slowTransport) SendBatch(frames []frame.Frame) error {
	s.sleep()
	return s.base.SendBatch(frames)
}

func (s *slowTransport) Recv() (byte, []byte, error) { return s.base.Recv() }
func (s *slowTransport) Close() error                { return s.base.Close() }

// newCongestedStripe builds a stripe wrapper whose primary lane starts fast
// (drained by a background reader), plus a dialLane that mints fresh pipes.
// Returns the wrapper, the dial count, and the throttle (call setDelay to
// congest after the baseline is established).
func newCongestedStripe(t *testing.T, udpActive func() bool) (*stripeTransport, *atomic.Int32, *slowTransport) {
	t.Helper()
	basePeer, baseServer := net.Pipe()
	t.Cleanup(func() { basePeer.Close() })
	// Drain everything the primary sends so writes complete after the delay.
	go func() {
		for {
			if _, _, err := frame.ReadFrame(basePeer); err != nil {
				return
			}
		}
	}()
	var dials atomic.Int32
	slow := &slowTransport{base: frame.NewStreamTransport(baseServer)}
	tr := newStripeTransport(slow, stripeConfig{
		dialLane: func() (uint32, frame.FrameTransport, error) {
			dials.Add(1)
			laneActive, lanePassive := net.Pipe()
			t.Cleanup(func() { laneActive.Close() })
			go func() {
				for {
					if _, _, err := frame.ReadFrame(laneActive); err != nil {
						return
					}
				}
			}()
			return uint32(dials.Load()), frame.NewStreamTransport(lanePassive), nil
		},
		udpActive: udpActive,
	})
	t.Cleanup(func() { tr.Close() })
	return tr, &dials, slow
}

// waitForBaselineEnough polls until the sojourn baseline has been
// established from enough fast samples to be trustworthy.
func waitForBaselineEnough(t *testing.T, tr *stripeTransport, minSamples uint64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		tr.ctrlMu.Lock()
		ok := tr.baseline > 0 && tr.sojournSamples >= minSamples
		tr.ctrlMu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("baseline not established after %v", 5*time.Second)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForLaneCount(t *testing.T, tr *stripeTransport, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for tr.laneCount() != want {
		if time.Now().After(deadline) {
			tr.ctrlMu.Lock()
			ema, base, n, last := tr.sojournEMA, tr.baseline, tr.sojournSamples, tr.lastSampleAt
			tr.ctrlMu.Unlock()
			t.Fatalf("laneCount = %d, want %d (gauge=%.2f ema=%.2fms base=%.2fms samples=%d lastSample=%v ago, events=%v)",
				tr.laneCount(), want, tr.currentGauge(), ema, base, n, time.Since(last), tr.snapshotEvents())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForEvent polls the controller ring buffer for a substring.
func waitForEvent(t *testing.T, tr *stripeTransport, substr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, e := range tr.snapshotEvents() {
			if strings.Contains(e, substr) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no event containing %q after %v (events=%v)", substr, 5*time.Second, tr.snapshotEvents())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestStripeBaselineFloorKeepsSentinel locks the sentinel fix: a same-tick
// sample (ms == 0, common on Windows' coarse clock) must clamp the baseline
// to the noise floor instead of leaving it at exactly 0 — a 0 baseline
// re-arms the "unbootstrapped" sentinel, and the next congested sample would
// re-base the line at congestion level, pinning gauge ≈ 0 forever.
func TestStripeBaselineFloorKeepsSentinel(t *testing.T) {
	_, baseServer := net.Pipe()
	defer baseServer.Close()
	tr := newStripeTransport(frame.NewStreamTransport(baseServer), stripeConfig{})
	defer tr.Close()

	floorMs := float64(stripeBaselineFloor) / float64(time.Millisecond)
	// Two back-to-back zero-delay samples; the second one lands on the
	// baseline-min branch with ms ≈ 0 on any clock granularity.
	tr.sampleSojourn(time.Now())
	tr.sampleSojourn(time.Now())

	tr.ctrlMu.Lock()
	base := tr.baseline
	tr.ctrlMu.Unlock()
	if base < floorMs {
		t.Fatalf("baseline = %v below noise floor %v (sentinel broken: same-tick sample left baseline ≈ 0)", base, floorMs)
	}

	// Congestion on top of the floored baseline must read as full gauge.
	tr.sampleSojourn(time.Now().Add(-100 * time.Millisecond))
	if g := tr.currentGauge(); g < 0.99 {
		t.Fatalf("gauge = %.2f, want ≈1.0 after congestion on floored baseline", g)
	}
}

func TestStripeControllerAddsLaneUnderCongestion(t *testing.T) {
	restore := stripeCtrlTestWindow()
	defer restore()

	tr, dials, slow := newCongestedStripe(t, nil)
	stop := make(chan struct{})
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := tr.Send(frame.FrameMeshPacket, synthTCPFlow(i), false); err != nil {
				return
			}
		}
	}()
	// Phase 1: fast traffic establishes the sliding-min baseline (§5.4).
	waitForBaselineEnough(t, tr, 10)
	// Phase 2: throttle to 60ms per send — sojourn ≈ 60× the sub-ms
	// baseline → gauge pins at 1.0 → sustained add.
	slow.setDelay(60 * time.Millisecond)
	waitForLaneCount(t, tr, 1)
	close(stop)
	if dials.Load() == 0 {
		t.Fatal("lane appeared without a controller dial")
	}
	waitForEvent(t, tr, "add lane")
}

func TestStripeControllerSuppressedByUDPActive(t *testing.T) {
	restore := stripeCtrlTestWindow()
	defer restore()

	tr, _, slow := newCongestedStripe(t, func() bool { return true })
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := tr.Send(frame.FrameMeshPacket, synthTCPFlow(i), false); err != nil {
				return
			}
		}
	}()
	waitForBaselineEnough(t, tr, 10)
	slow.setDelay(60 * time.Millisecond)
	// Wait well past the add window; the UDP leg owns the data path.
	time.Sleep(500 * time.Millisecond)
	if n := tr.laneCount(); n != 0 {
		t.Fatalf("laneCount = %d, want 0 while UDP leg Active", n)
	}
}

func TestStripeControllerRemovesLaneAfterClear(t *testing.T) {
	restore := stripeCtrlTestWindow()
	defer restore()

	tr, _, slow := newCongestedStripe(t, nil)
	stop := make(chan struct{})
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := tr.Send(frame.FrameMeshPacket, synthTCPFlow(i), false); err != nil {
				return
			}
		}
	}()
	waitForBaselineEnough(t, tr, 10)
	slow.setDelay(60 * time.Millisecond)
	waitForLaneCount(t, tr, 1)
	close(stop) // traffic stops → EMA pulled back to baseline → clear window elapses
	waitForLaneCount(t, tr, 0)
	waitForEvent(t, tr, "remove lane")
}

func TestStripeLaneProbeRecordsSRTTAndLoss(t *testing.T) {
	_, baseServer := net.Pipe()
	defer baseServer.Close()
	tr := newStripeTransport(frame.NewStreamTransport(baseServer), stripeConfig{})
	defer tr.Close()

	lanePeer, laneServer := net.Pipe()
	defer lanePeer.Close()
	tr.attachLane(1, frame.NewStreamTransport(laneServer))
	defer laneServer.Close()

	// One probe round: ping goes out on the lane, we answer like a remote
	// laneReadLoop would (echo timestamp+seq), srtt gets recorded.
	oldTimeout := stripeProbeTimeout
	stripeProbeTimeout = 80 * time.Millisecond
	defer func() { stripeProbeTimeout = oldTimeout }()

	tr.probeTick()
	ft, payload, err := frame.ReadFrame(lanePeer)
	if err != nil {
		t.Fatalf("probe ping read: %v", err)
	}
	if ft != frame.FrameLinkPing {
		t.Fatalf("probe frame = 0x%02x, want ping", ft)
	}
	var ping PingInfo
	if err := json.Unmarshal(payload, &ping); err != nil {
		t.Fatalf("probe decode: %v", err)
	}
	pong, _ := json.Marshal(PingInfo{Timestamp: ping.Timestamp, Seq: ping.Seq})
	if err := frame.WriteFrame(lanePeer, frame.FrameLinkPong, pong); err != nil {
		t.Fatalf("pong write: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		lanes := tr.snapshotLanes()
		if len(lanes) == 1 {
			lanes[0].probe.mu.Lock()
			srtt, sent := lanes[0].probe.srtt, lanes[0].probe.sent
			lanes[0].probe.mu.Unlock()
			if srtt > 0 && sent == 1 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("srtt not recorded after pong")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Second probe left unanswered: expiry counts it as loss. Expiry runs at
	// the next probeTick, so fire one after the timeout elapses.
	tr.probeTick()
	_, _, _ = frame.ReadFrame(lanePeer) // drain the ping, never answer
	time.Sleep(150 * time.Millisecond)  // > stripeProbeTimeout (80ms)
	tr.probeTick()                      // ages out the unanswered probe
	deadline = time.Now().Add(2 * time.Second)
	for {
		lanes := tr.snapshotLanes()
		if len(lanes) == 1 {
			lanes[0].probe.mu.Lock()
			lost := lanes[0].probe.lost
			lanes[0].probe.mu.Unlock()
			if lost == 1 {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("unanswered probe not counted as loss")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// --- rendezvous weighted dispatch (tasks 3.x) ---

// stripeTestWrapper builds a stripe wrapper over a dummy primary plus n
// lanes on dead-end pipes, returning the wrapper and the lane slice.
func stripeTestWrapper(t *testing.T, n int) (*stripeTransport, []*stripeLane) {
	t.Helper()
	_, baseServer := net.Pipe()
	tr := newStripeTransport(frame.NewStreamTransport(baseServer), stripeConfig{})
	t.Cleanup(func() { tr.Close() })
	for i := 0; i < n; i++ {
		_, laneServer := net.Pipe()
		tr.attachLane(uint32(i+1), frame.NewStreamTransport(laneServer))
	}
	return tr, tr.snapshotLanes()
}

// decideAll maps flowIndex → chosen lane ID (0 = primary) for the given lane set.
func decideAll(tr *stripeTransport, lanes []*stripeLane, flows int) map[int]uint32 {
	out := make(map[int]uint32, flows)
	for i := 0; i < flows; i++ {
		l := tr.pickLane(flowHash32(tr.salt, synthTCPFlow(i)), lanes)
		if l == nil {
			out[i] = 0
		} else {
			out[i] = l.id
		}
	}
	return out
}

func TestStripeRendezvousMinimalRemap(t *testing.T) {
	tr, lanes := stripeTestWrapper(t, 2)
	l1, l2 := lanes[0], lanes[1]
	const flows = 4096

	d1 := decideAll(tr, []*stripeLane{l1}, flows)     // {primary, l1}
	d2 := decideAll(tr, []*stripeLane{l1, l2}, flows) // add l2
	for f, old := range d1 {
		if new := d2[f]; new != old && new != l2.id {
			t.Fatalf("flow %d moved %d → %d on lane add (want %d or new lane %d)",
				f, old, new, old, l2.id)
		}
	}

	d3 := decideAll(tr, []*stripeLane{l2}, flows) // remove l1
	movedOffSurvivor := 0
	for f := 0; f < flows; f++ {
		if new := d3[f]; new != 0 && new != l2.id {
			t.Fatalf("flow %d landed on unknown lane %d after removal", f, new)
		}
		if d2[f] == l2.id && d3[f] != l2.id {
			movedOffSurvivor++
		}
	}
	if movedOffSurvivor > 0 {
		t.Fatalf("%d flows left the surviving lane on removal (minimal-remap violated)", movedOffSurvivor)
	}
}

func TestStripeWeightedDistribution(t *testing.T) {
	tr, lanes := stripeTestWrapper(t, 2)
	good, bad := lanes[0], lanes[1]
	// Poison the bad lane relative to its own baseline (§5.4/§六): base 10ms,
	// srtt 200ms → rel = 19 → weight 1/(1+19) = 0.05 vs neutral 1.0.
	bad.probe.mu.Lock()
	bad.probe.srtt = 200 * time.Millisecond
	bad.probe.srttBase = 10 * time.Millisecond
	bad.probe.mu.Unlock()

	const flows = 12000
	counts := map[uint32]int{0: 0, good.id: 0, bad.id: 0}
	for i := 0; i < flows; i++ {
		l := tr.pickLane(flowHash32(tr.salt, synthTCPFlow(i)), lanes)
		if l == nil {
			counts[0]++
		} else {
			counts[l.id]++
		}
	}
	// Expected ≈ 1 : 1 : 0.05 → good ≈ 49%, bad ≈ 2.4%.
	goodShare := float64(counts[good.id]) / flows
	badShare := float64(counts[bad.id]) / flows
	if goodShare < 0.38 || goodShare > 0.52 {
		t.Fatalf("good-lane share = %.2f, want ≈0.49", goodShare)
	}
	if badShare > 0.15 {
		t.Fatalf("bad-lane share = %.2f, want ≤0.15 (counts=%v)", badShare, counts)
	}
	if float64(counts[good.id]) < 3*float64(counts[bad.id]) {
		t.Fatalf("weight ratio not reflected: counts=%v", counts)
	}
}

// TestStripeAffinityPinnedAcrossWeightDrift (§3.6): once a flow's lane is
// pinned, probe-weight drift must not re-map it — the pre-fix behavior
// multiplied drifting currentWeight into the affinity score and flows near
// a score boundary flip-flopped across parallel channels, reordering the
// inner TCP stream.
func TestStripeAffinityPinnedAcrossWeightDrift(t *testing.T) {
	tr, lanes := stripeTestWrapper(t, 2)
	l1, l2 := lanes[0], lanes[1]

	// Find a flow that initially lands on l1.
	var flowPayload []byte
	for i := 0; i < 100000; i++ {
		p := synthTCPFlow(i)
		if tr.pickLanePinned(p, lanes) == l1 {
			flowPayload = p
			break
		}
	}
	if flowPayload == nil {
		t.Fatal("no flow landed on l1")
	}
	// Drift weights hard: l1 atrocious, l2 pristine. Rendezvous would flip.
	l1.probe.mu.Lock()
	l1.probe.srtt = 500 * time.Millisecond
	l1.probe.srttBase = 5 * time.Millisecond
	l1.probe.mu.Unlock()
	l2.probe.mu.Lock()
	l2.probe.srtt = 5 * time.Millisecond
	l2.probe.srttBase = 5 * time.Millisecond
	l2.probe.mu.Unlock()

	for i := 0; i < 8; i++ {
		if got := tr.pickLanePinned(flowPayload, lanes); got != l1 {
			t.Fatalf("pinned flow re-mapped to lane %v under weight drift (want l1=%d)", got, l1.id)
		}
	}

	// Lane removal drops the whole table: the flow must re-resolve onto a
	// surviving candidate without panic or stale lane.
	tr.removeLane(l1)
	lanes = tr.snapshotLanes()
	for i := 0; i < 8; i++ {
		got := tr.pickLanePinned(flowPayload, lanes)
		if got != nil && got != l2 {
			t.Fatalf("after l1 removal flow pinned to unknown lane %v (want nil or l2=%d)", got, l2.id)
		}
	}
}

func TestStripeDegradedLaneRemoved(t *testing.T) {
	restore := stripeCtrlTestWindow()
	defer restore()

	tr, _, slow := newCongestedStripe(t, nil)
	stop := make(chan struct{})
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := tr.Send(frame.FrameMeshPacket, synthTCPFlow(i), false); err != nil {
				return
			}
		}
	}()
	waitForBaselineEnough(t, tr, 10)
	slow.setDelay(60 * time.Millisecond)
	waitForLaneCount(t, tr, 1)
	close(stop)

	// Poison the lane's probe counters past the degradation gates.
	lanes := tr.snapshotLanes()
	if len(lanes) != 1 {
		t.Fatalf("lanes = %d, want 1", len(lanes))
	}
	lanes[0].probe.mu.Lock()
	lanes[0].probe.sent = 10
	lanes[0].probe.lost = 6 // 60% loss ≥ stripeLaneBadLoss
	lanes[0].probe.mu.Unlock()

	waitForLaneCount(t, tr, 0)
	waitForEvent(t, tr, "degraded")
}

// A full lane queue must not bounce a flow between lane and primary: the
// whole batch is immediately re-pinned to the primary lane (D10; no wait).
func TestStripeLaneFullRepinsToPrimary(t *testing.T) {
	tr, _ := stripeTestWrapper(t, 0)
	full := &stripeLane{id: 9, queue: make(chan []frame.Frame), done: make(chan struct{})}
	p := synthTCPFlow(1)
	key := flowHash32(tr.salt, p)
	tr.mu.Lock()
	tr.affinity[key] = full
	tr.mu.Unlock()

	ok, err := tr.laneEnqueue(full, []frame.Frame{{Type: frame.FrameMeshPacket, Payload: p}})
	if ok || err != nil {
		t.Fatalf("laneEnqueue on full lane = (%v, %v), want (false, nil)", ok, err)
	}
	tr.mu.Lock()
	l, pinned := tr.affinity[key]
	tr.mu.Unlock()
	if !pinned || l != nil {
		t.Fatalf("flow pin after full lane = (%v, %v), want primary (nil, true)", l, pinned)
	}
	if got := tr.pickLanePinned(p, []*stripeLane{full}); got != nil {
		t.Fatalf("next frame picked lane %v, want primary", got)
	}
}
