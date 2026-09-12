package tun

import (
	"fmt"
	"net"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/loopback"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

func TestNetstackLoopback(t *testing.T) {
	const tunNICID = 1
	const loNICID = 2

	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})

	// TUN NIC (channel endpoint)
	linkEP := channel.New(512, 1500, "")
	if err := s.CreateNIC(tunNICID, linkEP); err != nil {
		t.Fatalf("CreateNIC tun: %v", err)
	}

	// Loopback NIC
	loEP := loopback.New()
	if err := s.CreateNIC(loNICID, loEP); err != nil {
		t.Fatalf("CreateNIC loopback: %v", err)
	}

	// Register dnsAddr on loopback NIC
	dnsAddr := tcpip.AddrFrom4([4]byte{198, 18, 0, 3})
	ap := tcpip.AddressWithPrefix{Address: dnsAddr, PrefixLen: 32}
	protoAddr := tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: ap}
	if err := s.AddProtocolAddress(loNICID, protoAddr, stack.AddressProperties{}); err != nil {
		t.Fatalf("AddProtocolAddress dns: %v", err)
	}

	s.SetPromiscuousMode(tunNICID, true)
	s.SetSpoofing(tunNICID, true)
	s.SetPromiscuousMode(loNICID, true)
	s.SetSpoofing(loNICID, true)
	_ = s.SetForwardingDefaultAndAllNICs(ipv4.ProtocolNumber, true)

	// Route: dnsAddr + fakeIP range → loopback, default → tunNIC
	fakeIPSubnet, _ := tcpip.NewSubnet(tcpip.AddrFrom4([4]byte{198, 18, 0, 0}), tcpip.MaskFromBytes([]byte{255, 254, 0, 0}))
	dnsSubnet, _ := tcpip.NewSubnet(dnsAddr, tcpip.MaskFromBytes([]byte{255, 255, 255, 255}))
	s.SetRouteTable([]tcpip.Route{
		{Destination: dnsSubnet, NIC: loNICID},
		{Destination: fakeIPSubnet, NIC: loNICID},
		{Destination: header.IPv4EmptySubnet, NIC: tunNICID},
	})

	// TCP forwarder (catch-all)
	forwarderCalled := make(chan string, 1)
	fwd := tcp.NewForwarder(s, 0, 16, func(r *tcp.ForwarderRequest) {
		id := r.ID()
		dst := fmt.Sprintf("%s:%d", net.IP(id.LocalAddress.AsSlice()), id.LocalPort)
		t.Logf("[FORWARDER] called: dst=%s", dst)
		forwarderCalled <- dst
		var wq waiter.Queue
		ep, err := r.CreateEndpoint(&wq)
		if err != nil {
			r.Complete(true)
			return
		}
		r.Complete(false)
		ep.Close()
	})
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, fwd.HandlePacket)

	// UDP endpoint on dnsAddr:53 (simulating hijacker)
	var wq waiter.Queue
	udpEP, err := s.NewEndpoint(udp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
	if err != nil {
		t.Fatalf("NewEndpoint UDP: %v", err)
	}
	if err := udpEP.Bind(tcpip.FullAddress{NIC: loNICID, Addr: dnsAddr, Port: 53}); err != nil {
		t.Fatalf("UDP bind: %v", err)
	}
	defer udpEP.Close()

	// --- Test 1: TCP dial to local fakeIP (should go through loopback → forwarder) ---
	t.Log("=== Test 1: TCP dial to local fakeIP 198.18.0.4:8080 ===")
	go func() {
		conn, err := gonet.DialTCP(s, tcpip.FullAddress{
			NIC:  loNICID,
			Addr: tcpip.AddrFrom4([4]byte{198, 18, 0, 4}),
			Port: 8080,
		}, ipv4.ProtocolNumber)
		if err != nil {
			t.Logf("[DIAL] TCP error: %v", err)
			return
		}
		t.Logf("[DIAL] TCP connected: %s -> %s", conn.LocalAddr(), conn.RemoteAddr())
		conn.Close()
	}()

	select {
	case dst := <-forwarderCalled:
		t.Logf("[RESULT] SUCCESS: forwarder caught outbound TCP via loopback: %s", dst)
	case <-time.After(3 * time.Second):
		t.Logf("[RESULT] FAIL: forwarder NOT called")
	}

	// Check channel - should be empty (packet went to loopback, not tunNIC)
	pkt := linkEP.Read()
	if pkt != nil {
		buf := pkt.ToBuffer()
		data := buf.Flatten()
		if len(data) >= 20 {
			t.Logf("[CHANNEL] UNEXPECTED packet in tunNIC channel: src=%s dst=%s proto=%d",
				net.IP(data[12:16]), net.IP(data[16:20]), data[9])
		}
		pkt.DecRef()
	} else {
		t.Logf("[CHANNEL] tunNIC channel empty (correct - packet went to loopback)")
	}

	// --- Test 2: UDP dial to dnsAddr:53 (should go through loopback → hijacker) ---
	t.Log("=== Test 2: UDP dial to dnsAddr 198.18.0.3:53 ===")
	udpConn, udpDialErr := gonet.DialUDP(s, nil, &tcpip.FullAddress{
		Addr: dnsAddr,
		Port: 53,
	}, ipv4.ProtocolNumber)
	if udpDialErr != nil {
		t.Fatalf("DialUDP: %v", udpDialErr)
	}
	defer udpConn.Close()

	// Register waiter BEFORE writing — delivery is synchronous via loopback
	waitEntry, ch := waiter.NewChannelEntry(waiter.EventIn)
	wq.EventRegister(&waitEntry)
	defer wq.EventUnregister(&waitEntry)

	if _, err := udpConn.Write([]byte("test")); err != nil {
		t.Logf("[DIAL] UDP write error: %v", err)
	} else {
		t.Logf("[DIAL] UDP write ok")
	}

	select {
	case <-ch:
		t.Logf("[RESULT] SUCCESS: hijacker received UDP via loopback")
	case <-time.After(2 * time.Second):
		t.Logf("[RESULT] FAIL: hijacker did NOT receive UDP")
	}

	// --- Test 3: UDP to NON-local address (should go to tunNIC channel) ---
	t.Log("=== Test 3: UDP to non-local 8.8.8.8:53 (should go to tunNIC) ===")

	// Drain any leftover packets from channel
	for linkEP.Read() != nil {
	}

	udpConn2, err2 := gonet.DialUDP(s, nil, &tcpip.FullAddress{
		Addr: tcpip.AddrFrom4([4]byte{8, 8, 8, 8}),
		Port: 53,
	}, ipv4.ProtocolNumber)
	if err2 != nil {
		t.Logf("[DIAL] UDP to 8.8.8.8 error: %v", err2)
	} else {
		defer udpConn2.Close()
		udpConn2.Write([]byte("test"))
		t.Logf("[DIAL] UDP write to 8.8.8.8 ok")
	}

	time.Sleep(500 * time.Millisecond)

	pkt2 := linkEP.Read()
	if pkt2 != nil {
		buf2 := pkt2.ToBuffer()
		data := buf2.Flatten()
		if len(data) >= 20 {
			t.Logf("[CHANNEL] SUCCESS: non-local packet went to tunNIC: src=%s dst=%s proto=%d",
				net.IP(data[12:16]), net.IP(data[16:20]), data[9])
		}
		pkt2.DecRef()
	} else {
		t.Logf("[CHANNEL] FAIL: no packet in tunNIC channel")
	}

	s.Close()
	t.Log("=== All tests complete ===")
}
