package mesh

import (
	"io"
	"net"
	"sync"
	"testing"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

// mockPeerSender implements PeerSender for testing.
type mockPeerSender struct {
	mu     sync.Mutex
	sent   [][]byte
	closed bool
	nodeID string
}

func newMockPeerSender(nodeID string) *mockPeerSender {
	return &mockPeerSender{nodeID: nodeID}
}

func (p *mockPeerSender) Send(data []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return io.ErrClosedPipe
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	p.sent = append(p.sent, cp)
	return nil
}

func (p *mockPeerSender) SendGossip(data []byte) {}

func (p *mockPeerSender) GetNodeID() string {
	return p.nodeID
}

func TestHTunnelEndpointWritePackets(t *testing.T) {
	peer := newMockPeerSender("test-node")
	ep := NewHTunnelEndpoint(100, peer)
	defer ep.Close()

	ipPkt := buildMinimalIPv4Packet(net.IP{100, 179, 0, 3}, net.IP{10, 0, 0, 1})
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(ipPkt),
	})

	var pkts stack.PacketBufferList
	pkts.PushBack(pkt)

	n, err := ep.WritePackets(pkts)
	if err != nil {
		t.Fatalf("WritePackets error: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 packet written, got %d", n)
	}

	peer.mu.Lock()
	if len(peer.sent) != 1 {
		t.Fatalf("expected 1 packet sent via peer, got %d", len(peer.sent))
	}
	sentData := peer.sent[0]
	peer.mu.Unlock()

	if len(sentData) != len(ipPkt) {
		t.Fatalf("payload length mismatch: got %d, want %d", len(sentData), len(ipPkt))
	}
	for i := range ipPkt {
		if sentData[i] != ipPkt[i] {
			t.Fatalf("payload[%d] mismatch: got %02x, want %02x", i, sentData[i], ipPkt[i])
		}
	}
}

func TestHTunnelEndpointMultiplePackets(t *testing.T) {
	peer := newMockPeerSender("test-node")
	ep := NewHTunnelEndpoint(100, peer)
	defer ep.Close()

	var pkts stack.PacketBufferList
	for i := 0; i < 5; i++ {
		ipPkt := buildMinimalIPv4Packet(net.IP{100, 179, 0, 3}, net.IP{10, 0, 0, byte(i + 1)})
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
			Payload: buffer.MakeWithData(ipPkt),
		})
		pkts.PushBack(pkt)
	}

	n, err := ep.WritePackets(pkts)
	if err != nil {
		t.Fatalf("WritePackets error: %v", err)
	}
	if n != 5 {
		t.Fatalf("expected 5 packets written, got %d", n)
	}

	peer.mu.Lock()
	if len(peer.sent) != 5 {
		t.Fatalf("expected 5 packets sent via peer, got %d", len(peer.sent))
	}
	peer.mu.Unlock()
}

func TestHTunnelEndpointInNetstack(t *testing.T) {
	peer := newMockPeerSender("test-node")

	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol},
	})

	mainEP := channel.New(512, 1500, "")
	if err := s.CreateNIC(1, mainEP); err != nil {
		t.Fatalf("CreateNIC main: %v", err)
	}

	htEP := NewHTunnelEndpoint(100, peer)
	if err := s.CreateNIC(100, htEP); err != nil {
		t.Fatalf("CreateNIC htunnel: %v", err)
	}
	s.SetPromiscuousMode(100, true)
	s.SetSpoofing(100, true)

	ap := tcpip.AddressWithPrefix{Address: tcpip.AddrFrom4([4]byte{100, 179, 0, 3}), PrefixLen: 32}
	s.AddProtocolAddress(1, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: ap,
	}, stack.AddressProperties{})

	s.SetPromiscuousMode(1, true)
	s.SetSpoofing(1, true)
	_ = s.SetForwardingDefaultAndAllNICs(ipv4.ProtocolNumber, true)

	s.SetRouteTable([]tcpip.Route{
		{Destination: header.IPv4EmptySubnet, NIC: 1},
	})

	if htEP.MTU() != 1500 {
		t.Errorf("MTU = %d, want 1500", htEP.MTU())
	}
	if htEP.MaxHeaderLength() != 0 {
		t.Errorf("MaxHeaderLength = %d, want 0", htEP.MaxHeaderLength())
	}
	if !htEP.IsAttached() {
		t.Error("IsAttached should return true")
	}

	htEP.Close()
}

func buildMinimalIPv4Packet(src, dst net.IP) []byte {
	src4 := src.To4()
	dst4 := dst.To4()
	pkt := make([]byte, 20)
	pkt[0] = 0x45
	pkt[8] = 64
	pkt[9] = 6
	copy(pkt[12:16], src4)
	copy(pkt[16:20], dst4)
	return pkt
}
