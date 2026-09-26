package tun

import (
	"io"
	"net"
	"sync"
	"testing"

	"phaethon/frame"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

// mockTransport implements frame.FrameTransport for testing.
type mockTransport struct {
	mu      sync.Mutex
	sent    []mockFrame
	closed  bool
	closeCh chan struct{}
}

type mockFrame struct {
	frameType byte
	payload   []byte
}

func newMockTransport() *mockTransport {
	return &mockTransport{closeCh: make(chan struct{})}
}

func (t *mockTransport) Send(frameType byte, payload []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return io.ErrClosedPipe
	}
	data := make([]byte, len(payload))
	copy(data, payload)
	t.sent = append(t.sent, mockFrame{frameType: frameType, payload: data})
	return nil
}

func (t *mockTransport) Recv() (byte, []byte, error) {
	<-t.closeCh
	return 0, nil, io.ErrClosedPipe
}

func (t *mockTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed {
		t.closed = true
		close(t.closeCh)
	}
	return nil
}

func TestHTunnelEndpointWritePackets(t *testing.T) {
	transport := newMockTransport()
	ep := newHTunnelEndpoint(100, transport)
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

	transport.mu.Lock()
	if len(transport.sent) != 1 {
		t.Fatalf("expected 1 frame sent, got %d", len(transport.sent))
	}
	f := transport.sent[0]
	transport.mu.Unlock()

	if f.frameType != frame.FrameMeshPacket {
		t.Fatalf("expected FrameMeshPacket (%d), got %d", frame.FrameMeshPacket, f.frameType)
	}
	if len(f.payload) != len(ipPkt) {
		t.Fatalf("payload length mismatch: got %d, want %d", len(f.payload), len(ipPkt))
	}
	for i := range ipPkt {
		if f.payload[i] != ipPkt[i] {
			t.Fatalf("payload[%d] mismatch: got %02x, want %02x", i, f.payload[i], ipPkt[i])
		}
	}
}

func TestHTunnelEndpointMultiplePackets(t *testing.T) {
	transport := newMockTransport()
	ep := newHTunnelEndpoint(100, transport)
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

	transport.mu.Lock()
	if len(transport.sent) != 5 {
		t.Fatalf("expected 5 frames sent, got %d", len(transport.sent))
	}
	transport.mu.Unlock()
}

func TestHTunnelEndpointInNetstack(t *testing.T) {
	transport := newMockTransport()

	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol},
	})

	mainEP := channel.New(512, 1500, "")
	if err := s.CreateNIC(1, mainEP); err != nil {
		t.Fatalf("CreateNIC main: %v", err)
	}

	htEP := newHTunnelEndpoint(100, transport)
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
