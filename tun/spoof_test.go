package tun

import (
	"encoding/binary"
	"net"
	"testing"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// TestHijackerCrossNodeRedirect simulates the hijacker cross-node DNS redirect:
// 1. A DNS query arrives at hijacker with src=VIP:mappedPort, dst=dnsAddr:53
// 2. Hijacker creates a new socket bound to VIP:mappedPort
// 3. Sends the query to remote GIP:53
// 4. Verifies the outbound packet has correct src IP, src port, dst IP, dst port
func TestHijackerCrossNodeRedirect(t *testing.T) {
	linkEP := channel.New(16, 1500, "")
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{udp.NewProtocol},
	})

	if err := s.CreateNIC(1, linkEP); err != nil {
		t.Fatalf("create nic: %v", err)
	}

	vipAddr := tcpip.AddrFrom4([4]byte{100, 64, 0, 1})
	dnsAddr := tcpip.AddrFrom4([4]byte{100, 64, 0, 3})
	remoteGIP := tcpip.AddrFrom4([4]byte{100, 64, 1, 3})

	for _, addr := range []tcpip.AddressWithPrefix{
		{Address: vipAddr, PrefixLen: 32},
		{Address: dnsAddr, PrefixLen: 32},
	} {
		if err := s.AddProtocolAddress(1, tcpip.ProtocolAddress{
			Protocol:          ipv4.ProtocolNumber,
			AddressWithPrefix: addr,
		}, stack.AddressProperties{}); err != nil {
			t.Fatalf("add address %s: %v", addr, err)
		}
	}

	s.SetSpoofing(1, true)
	s.SetPromiscuousMode(1, true)
	s.SetRouteTable([]tcpip.Route{
		{Destination: header.IPv4EmptySubnet, NIC: 1},
	})

	mappedPort := uint16(40001)
	payload := []byte("dns-query-payload")

	// Simulate hijacker: create socket bound to VIP:mappedPort
	var wq waiter.Queue
	ep, err := s.NewEndpoint(udp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
	if err != nil {
		t.Fatalf("new endpoint: %v", err)
	}

	// Bind to VIP:mappedPort (this is the key: specific IP + specific port)
	if err := ep.Bind(tcpip.FullAddress{Addr: vipAddr, Port: mappedPort}); err != nil {
		t.Fatalf("bind to VIP:%d: %v", mappedPort, err)
	}
	t.Logf("socket bound to %s:%d", vipAddr, mappedPort)

	// Connect to remote GIP:53
	if err := ep.Connect(tcpip.FullAddress{NIC: 1, Addr: remoteGIP, Port: 53}); err != nil {
		t.Fatalf("connect to remote GIP: %v", err)
	}

	// Send the DNS query
	if _, err := ep.Write(&slicePayload{data: payload}, tcpip.WriteOptions{}); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Log("packet sent, closing socket (hijacker done)")
	ep.Close()

	// Read outbound packet from linkEP
	pkt := linkEP.Read()
	if pkt == nil {
		t.Fatal("no outbound packet — socket failed to send via netstack")
	}
	defer pkt.DecRef()

	buf := pkt.ToBuffer()
	data := buf.Flatten()
	if len(data) < 20 {
		t.Fatalf("packet too short: %d bytes", len(data))
	}

	// Parse IP header
	srcIP := net.IP(data[12:16])
	dstIP := net.IP(data[16:20])
	ipHeaderLen := int(data[0]&0x0f) * 4

	// Parse UDP header
	udpStart := ipHeaderLen
	if len(data) < udpStart+8 {
		t.Fatalf("packet too short for UDP header: %d bytes", len(data))
	}
	srcPort := binary.BigEndian.Uint16(data[udpStart : udpStart+2])
	dstPort := binary.BigEndian.Uint16(data[udpStart+2 : udpStart+4])
	udpPayload := data[udpStart+8:]

	t.Logf("outbound packet:")
	t.Logf("  src: %s:%d", srcIP, srcPort)
	t.Logf("  dst: %s:%d", dstIP, dstPort)
	t.Logf("  payload: %d bytes", len(udpPayload))

	// Verify all fields
	wantSrcIP := net.IP{100, 64, 0, 1}
	wantDstIP := net.IP{100, 64, 1, 3}

	if !srcIP.Equal(wantSrcIP) {
		t.Errorf("src IP = %s, want %s", srcIP, wantSrcIP)
	}
	if srcPort != mappedPort {
		t.Errorf("src port = %d, want %d", srcPort, mappedPort)
	}
	if !dstIP.Equal(wantDstIP) {
		t.Errorf("dst IP = %s, want %s", dstIP, wantDstIP)
	}
	if dstPort != 53 {
		t.Errorf("dst port = %d, want 53", dstPort)
	}
	if string(udpPayload) != string(payload) {
		t.Errorf("payload mismatch")
	}

	t.Log("PASS: hijacker socket can bind to VIP:mappedPort and send with correct src/dst")
}
