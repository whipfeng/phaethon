package p2p

import (
	"bytes"
	"net"
	"testing"
	"time"

	"phaethon/frame"
)

// stripeTransport skeleton (tasks 1.2): one primary lane must behave
// byte-for-byte like the unwrapped base transport.

func TestStripeTransportSendPassThrough(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	tr := newStripeTransport(frame.NewStreamTransport(server))
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
	tr := newStripeTransport(frame.NewStreamTransport(server))
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
	tr := newStripeTransport(frame.NewStreamTransport(server))
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
	tr := newStripeTransport(frame.NewStreamTransport(server))
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
	tr := newStripeTransport(base)

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
	tr := newStripeTransport(frame.NewStreamTransport(server))
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
	tr := newStripeTransport(frame.NewStreamTransport(server))
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

// readFramesAsync reads n frames from conn, reporting each with index idx.
func readFramesAsync(idx int, conn net.Conn, n int, out chan<- pipeRead) {
	for i := 0; i < n; i++ {
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
	tr := newStripeTransport(base)
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
	go readFramesAsync(0, conns[0].peerEnd, flows*2+2, got) // primary: overflow + unsplittable
	go readFramesAsync(1, conns[1].peerEnd, flows*2, got)
	go readFramesAsync(2, conns[2].peerEnd, flows*2, got)

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
	tr := newStripeTransport(frame.NewStreamTransport(baseServer))
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

	// Link ping on a lane is accepted as control.
	go func() {
		_ = frame.WriteFrame(lanePeer, frame.FrameLinkPing, []byte("probe"))
	}()
	ft, payload, err = tr.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if ft != frame.FrameLinkPing || !bytes.Equal(payload, []byte("probe")) {
		t.Fatalf("got (%#x,%q)", ft, payload)
	}
}

func TestStripeTransportLaneControlDropped(t *testing.T) {
	_, baseServer := net.Pipe()
	defer baseServer.Close()
	tr := newStripeTransport(frame.NewStreamTransport(baseServer))
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
	tr := newStripeTransport(frame.NewStreamTransport(baseServer))
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
	passiveStripe := newStripeTransport(frame.NewStreamTransport(sessServer))
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
	activeStripe := newStripeTransport(frame.NewStreamTransport(activeBaseServer))
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
	mgr.stripeAttachLane(activePeer, fakeDialer)
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
