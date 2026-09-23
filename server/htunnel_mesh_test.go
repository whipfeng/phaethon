package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"phaethon/config"
	"phaethon/dialer"
	"phaethon/frame"
	"phaethon/mesh"
	"phaethon/p2p"
)

// meshTestHandler is a p2p.MeshHandler stub capturing hello registrations and
// echoing mesh frames back to the sender.
type meshTestHandler struct {
	mu     sync.Mutex
	send   mesh.PeerSender
	nodeID string

	regCh   chan string
	meshGot chan []byte
}

func (h *meshTestHandler) HandleMeshFrame(fromNodeID string, f []byte) {
	h.mu.Lock()
	s := h.send
	h.mu.Unlock()
	h.meshGot <- f
	if s != nil {
		_ = s.Send(f) // echo back to the client
	}
}

func (h *meshTestHandler) HandleTopologyGossip(sender mesh.PeerSender, data []byte) {}

func (h *meshTestHandler) RegisterPeer(sender mesh.PeerSender) {
	h.mu.Lock()
	h.send = sender
	h.nodeID = sender.GetNodeID()
	h.mu.Unlock()
	h.regCh <- sender.GetNodeID()
}

func (h *meshTestHandler) UnregisterPeer(sender mesh.PeerSender) {
	h.mu.Lock()
	if h.send == sender {
		h.send = nil
	}
	h.mu.Unlock()
}

func (h *meshTestHandler) UnregisterPeerByNodeID(nodeID string) {
	h.mu.Lock()
	if h.nodeID == nodeID {
		h.send = nil
		h.nodeID = ""
	}
	h.mu.Unlock()
}

func (h *meshTestHandler) BuildGossipInfo() *mesh.GossipInfo {
	return &mesh.GossipInfo{
		ClaimedSubnets: []mesh.GossipClaimedSubnet{
			{Subnet: "100.200.0.0/16", NodeID: "srv-node", Hop: 0},
		},
	}
}

// TestHTunnelMeshChannel_E2E runs a full loopback: client direct transport →
// server mesh channel → real P2P session → stub mesh handler, verifying hello
// exchange, mesh packet echo and channel teardown.
func TestHTunnelMeshChannel_E2E(t *testing.T) {
	handler := &meshTestHandler{
		regCh:   make(chan string, 4),
		meshGot: make(chan []byte, 4),
	}
	mgr := p2p.NewP2PManager("srv-node", "test", nil)
	mgr.SetMeshHandler(handler)

	oldMgr := p2p.GlobalP2PManager
	p2p.GlobalP2PManager = mgr
	defer func() { p2p.GlobalP2PManager = oldMgr }()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	srv := &HTunnelServer{
		BaseServer: BaseServer{
			RuleConf: &config.RuleConfiguration{},
			Mapping:  &config.Mapping{Name: "ht-mesh-test", Port: port},
		},
		Password: "testpw",
	}
	httpSrv := &http.Server{Handler: srv}
	go func() { _ = httpSrv.Serve(ln) }()
	defer httpSrv.Close()

	d := &dialer.HTunnelDialer{BaseDialer: dialer.BaseDialer{Proxy: &config.Proxy{
		Name:     "test-ht",
		Type:     "h_tunnel",
		URL:      fmt.Sprintf("http://127.0.0.1:%d/", port),
		Password: "testpw",
	}}}

	tr, err := d.DialP2P()
	if err != nil {
		t.Fatalf("DialP2P: %v", err)
	}
	defer tr.Close()

	// Client hello (real P2P sessions send this immediately).
	hello := mesh.GossipInfo{
		Cmd:             "hello",
		ProtocolVersion: p2p.P2PProtocolVersion,
		ClaimedSubnets: []mesh.GossipClaimedSubnet{
			{Subnet: "100.201.0.0/16", NodeID: "cli-node", Hop: 0},
		},
	}
	helloData, _ := json.Marshal(hello)
	if err := tr.Send(frame.FrameData, helloData); err != nil {
		t.Fatalf("send hello: %v", err)
	}

	// Server session must register the client node.
	select {
	case nodeID := <-handler.regCh:
		if nodeID != "cli-node" {
			t.Fatalf("registered nodeID = %q, want cli-node", nodeID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not register client hello")
	}

	// Server hello must arrive at the client.
	ft, payload, err := tr.Recv()
	if err != nil {
		t.Fatalf("recv server hello: %v", err)
	}
	if ft != frame.FrameData {
		t.Fatalf("expected FrameData, got 0x%02x", ft)
	}
	var srvHello mesh.GossipInfo
	if err := json.Unmarshal(payload, &srvHello); err != nil {
		t.Fatalf("parse server hello: %v", err)
	}
	if srvHello.Cmd != "hello" || srvHello.ProtocolVersion != p2p.P2PProtocolVersion {
		t.Fatalf("bad server hello: %+v", srvHello)
	}
	hop0 := false
	for _, cs := range srvHello.ClaimedSubnets {
		if cs.Hop == 0 && cs.NodeID == "srv-node" {
			hop0 = true
		}
	}
	if !hop0 {
		t.Fatalf("server hello missing hop=0 nodeID srv-node: %+v", srvHello)
	}

	// Mesh packet round trip: client → server handler → echo → client.
	echoPayload := []byte("mesh-echo-test")
	if err := tr.Send(frame.FrameMeshPacket, echoPayload); err != nil {
		t.Fatalf("send mesh packet: %v", err)
	}
	select {
	case got := <-handler.meshGot:
		if !bytes.Equal(got, echoPayload) {
			t.Fatalf("mesh payload mismatch: %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not receive mesh packet")
	}

	echoDeadline := time.Now().Add(5 * time.Second)
	var echoed []byte
	for {
		if time.Now().After(echoDeadline) {
			t.Fatal("no echoed mesh packet")
		}
		ft, payload, err := tr.Recv()
		if err != nil {
			t.Fatalf("recv echo: %v", err)
		}
		if ft == frame.FrameMeshPacket {
			echoed = payload
			break
		}
		// Skip interleaved heartbeats/gossip; keep polling.
	}
	if !bytes.Equal(echoed, echoPayload) {
		t.Fatalf("echo payload mismatch: %q", echoed)
	}

	// Teardown: client DELETE must close the server channel.
	if err := tr.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}
