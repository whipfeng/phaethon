package p2p

import (
	"math"
	"testing"

	"phaethon/frame"
)

func wrapIPIP(inner []byte) []byte {
	p := make([]byte, 20+len(inner))
	p[0] = 0x45
	p[9] = 4
	p[12], p[13], p[14], p[15] = 100, 2, 0, 1
	p[16], p[17], p[18], p[19] = 100, 64, 0, 1
	copy(p[20:], inner)
	return p
}

func synthTCPSeg(sport uint16, flags byte, payload int) []byte {
	p := make([]byte, 40+payload)
	p[0] = 0x45
	p[9] = 6
	p[12], p[13], p[14], p[15] = 172, 30, 0, 2
	p[16], p[17], p[18], p[19] = 10, 193, 131, 25
	p[20], p[21] = byte(sport>>8), byte(sport)
	p[22], p[23] = 0x01, 0xBB
	p[32] = 5 << 4
	p[33] = flags
	return p
}

func TestFlowHash32_IPIPUsesInnerFlow(t *testing.T) {
	const salt = 0x5a5a1234
	a1 := wrapIPIP(synthTCPSeg(40000, 0x18, 1000))
	a2 := wrapIPIP(synthTCPSeg(40000, 0x10, 0))
	b := wrapIPIP(synthTCPSeg(40001, 0x18, 1000))
	if flowHash32(salt, a1) != flowHash32(salt, a2) {
		t.Fatal("same inner flow hashed differently")
	}
	if flowHash32(salt, a1) == flowHash32(salt, b) {
		t.Fatal("different inner flows collapsed onto one hash")
	}
	if flowHash32(salt, a1) != flowHash32(salt, synthTCPSeg(40000, 0x18, 1000)) {
		t.Fatal("IPIP hash should equal native inner hash")
	}
}

func TestIsDNSPacket_IPIP(t *testing.T) {
	if !isDNSPacket(wrapIPIP(synthDNSPacket())) {
		t.Fatal("IPIP DNS not classified as DNS")
	}
	if isDNSPacket(wrapIPIP(synthTCPSeg(40000, 0x18, 100))) {
		t.Fatal("IPIP TCP classified as DNS")
	}
}

// Fragmented or truncated IPIP falls back to the outer header without
// panicking: outer proto 4 is not DNS.
func TestClassifiers_IPIPFallback(t *testing.T) {
	frag := wrapIPIP(synthTCPSeg(40000, 0x10, 0))
	frag[6] = 0x20
	trunc := wrapIPIP(synthTCPSeg(40000, 0x10, 0))[:35]
	for name, p := range map[string][]byte{"fragment": frag, "truncated": trunc} {
		if isDNSPacket(p) {
			t.Errorf("%s: outer IPIP misclassified", name)
		}
		outer := append([]byte(nil), p[:20]...)
		if flowHash32(1, p) != flowHash32(1, outer) {
			t.Errorf("%s: hash did not fall back to outer header", name)
		}
	}
}

func newWriteTestPeer() *Peer {
	var q [256]chan writeReq
	for i := range q {
		q[i] = make(chan writeReq, dataQueueMaxDepth)
	}
	p := &Peer{
		ID:         "t",
		dnsQueue:   make(chan writeReq, 512),
		dataQueues: q,
		drrOutput:  make(chan writeReq, 512),
		controlCh:  make(chan writeReq, 512),
		stopCh:     make(chan struct{}),
	}
	p.drrScheduler = newDRRScheduler(&q, &p.dataBudget)
	p.dataQueuesReady.Store(true)
	p.dataBudget.Store(dataQueueByteBudget)
	return p
}

// Pure ACKs (native or IPIP) ride the DRR data queues: there is no ACK
// priority queue any more.
func TestEnqueue_PureACKGoesToDRR(t *testing.T) {
	p := newWriteTestPeer()
	ack := wrapIPIP(synthTCPSeg(40000, 0x10, 0))
	if err := enqueueWrite(p, frame.FrameMeshPacket, ack, false); err != nil {
		t.Fatal(err)
	}
	idx := p.drrScheduler.hashFlow(ack)
	if len(p.dataQueues[idx]) != 1 || len(p.dnsQueue) != 0 || len(p.controlCh) != 0 {
		t.Fatalf("pure ACK not in its DRR queue (data=%d dns=%d ctrl=%d)",
			len(p.dataQueues[idx]), len(p.dnsQueue), len(p.controlCh))
	}
}

func TestCollectWriteBatch_PriorityOrder(t *testing.T) {
	p := newWriteTestPeer()
	data := func(c byte) writeReq { return writeReq{frameType: frame.FrameMeshPacket, data: []byte{c}} }
	p.drrOutput <- data('2')
	p.drrOutput <- data('3')
	p.dnsQueue <- writeReq{frameType: frame.FrameMeshPacket, data: []byte{'d'}, dns: true}
	p.controlCh <- writeReq{frameType: frame.FrameLinkPong, data: []byte{'c'}, isControl: true}

	b := collectWriteBatch(p, data('1'), 64, math.MaxInt64)
	got := ""
	for _, r := range b {
		got += string(r.data)
	}
	if got != "cd123" {
		t.Fatalf("batch order = %q, want control, DNS, then data in FIFO order (cd123)", got)
	}
	if !b[0].isControl || b[1].isControl {
		t.Fatal("control flag lost")
	}
}

func TestCollectWriteBatch_RespectsMax(t *testing.T) {
	p := newWriteTestPeer()
	for i := 0; i < 10; i++ {
		p.controlCh <- writeReq{frameType: frame.FrameLinkPong, isControl: true}
		p.drrOutput <- writeReq{frameType: frame.FrameMeshPacket}
	}
	b := collectWriteBatch(p, writeReq{frameType: frame.FrameMeshPacket}, 8, math.MaxInt64)
	if len(b) != 8 {
		t.Fatalf("batch len = %d, want 8", len(b))
	}
	for i := 0; i < 7; i++ {
		if !b[i].isControl {
			t.Fatalf("frame %d not control: control frames must fill first", i)
		}
	}
	if len(p.controlCh) != 3 || len(p.drrOutput) != 10 {
		t.Fatalf("left ctrl=%d data=%d, want 3/10", len(p.controlCh), len(p.drrOutput))
	}
}
