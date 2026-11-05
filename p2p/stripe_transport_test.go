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
