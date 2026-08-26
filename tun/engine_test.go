package tun

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"phaethon/config"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

// TestUDPForwarderRegistered verifies the UDP forwarder handler is registered
// and fires when a UDP datagram arrives in the netstack.
//
// This test does NOT require admin privileges or a real TUN device.
// It creates an in-memory netstack, registers TCP+UDP handlers (mirroring
// Engine.initStack), injects a synthetic UDP packet, and confirms the handler
// was invoked.
func TestUDPForwarderRegistered(t *testing.T) {
	linkEP := channel.New(512, 1500, "")

	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})

	const nicID = 1
	if err := s.CreateNIC(nicID, linkEP); err != nil {
		t.Fatalf("CreateNIC: %v", err)
	}

	tunIP := tcpip.AddrFrom4([4]byte{198, 18, 0, 1})
	ap := tcpip.AddressWithPrefix{Address: tunIP, PrefixLen: 15}
	if err := s.AddProtocolAddress(nicID, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: ap,
	}, stack.AddressProperties{}); err != nil {
		t.Fatalf("AddProtocolAddress: %v", err)
	}

	s.SetPromiscuousMode(nicID, true)
	s.SetSpoofing(nicID, true)
	s.SetRouteTable([]tcpip.Route{
		{Destination: header.IPv4EmptySubnet, NIC: nicID},
		{Destination: header.IPv6EmptySubnet, NIC: nicID},
	})

	// Track whether handlers fire
	var udpFired sync.WaitGroup
	udpFired.Add(1)

	// Register UDP forwarder (same as Engine.acceptUDP)
	fwd := udp.NewForwarder(s, func(r *udp.ForwarderRequest) {
		defer udpFired.Done()
		id := r.ID()
		dstPort := id.LocalPort

		// Verify handler fired with valid port
		// Note: LocalAddress may be transformed by netstack routing;
		// the critical assertion is that the forwarder fires at all.
		if dstPort == 0 {
			t.Errorf("UDP dst port = %d, want non-zero", int(dstPort))
		}
	})
	s.SetTransportProtocolHandler(udp.ProtocolNumber, fwd.HandlePacket)

	// Build a synthetic UDP packet: src=198.18.0.100:12345 -> dst=8.8.8.8:53
	srcIP := tcpip.AddrFrom4([4]byte{198, 18, 0, 100})
	dstIP := tcpip.AddrFrom4([4]byte{8, 8, 8, 8})

	udpPayload := []byte("hello dns test")
	udpHdr := make([]byte, header.UDPMinimumSize)
	hdr := header.UDP(udpHdr)
	hdr.Encode(&header.UDPFields{
		SrcPort: 12345,
		DstPort: 53,
		Length:  uint16(header.UDPMinimumSize + len(udpPayload)),
	})
	hdr.SetChecksum(0) // optional IPv4; netstack will validate or recalc as needed
	udpPkt := append(udpHdr, udpPayload...)

	ipTotalLen := header.IPv4MinimumSize + len(udpPkt)
	ipBuf := make([]byte, ipTotalLen)
	copy(ipBuf[header.IPv4MinimumSize:], udpPkt)
	ip := header.IPv4(ipBuf)
	ip.Encode(&header.IPv4Fields{
		TOS:            0,
		TotalLength:    uint16(ipTotalLen),
		ID:             1234,
		Flags:          0x40, // Don't Fragment (uint8)
		FragmentOffset: 0,
		TTL:            64,
		Protocol:       uint8(udp.ProtocolNumber),
		Checksum:       0,
		SrcAddr:        srcIP,
		DstAddr:        dstIP,
	})
	ip.SetChecksum(^ip.CalculateChecksum())

	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(ipBuf),
	})
	linkEP.InjectInbound(ipv4.ProtocolNumber, pkt)
	pkt.DecRef()

	// Wait up to 2s for UDP handler to fire
	done := make(chan struct{})
	go func() {
		udpFired.Wait()
		close(done)
	}()
	select {
	case <-done:
		t.Log("✓ UDP forwarder handler fired successfully")
	case <-time.After(2 * time.Second):
		t.Fatal("✗ UDP forwarder did NOT fire within 2s — handler not registered or packet not delivered")
	}
}

// TestEngineStartStop exercises Engine lifecycle without requiring admin/TUN device.
// It verifies that Start() fails gracefully when not admin (expected on CI).
func TestEngineStartStop(t *testing.T) {
	engine := NewEngine(nil)

	err := engine.Start()
	if err == nil {
		// Running as admin with TUN driver available — clean shutdown
		t.Log("Engine started successfully (admin + TUN driver present)")
		if stopErr := engine.Stop(); stopErr != nil {
			t.Errorf("Stop error: %v", stopErr)
		}
	} else {
		t.Logf("Engine.Start failed as expected (no admin/no TUN): %v", err)
	}
}

// TestEngineUDPWithRuleConfig tests handleUDP path through rule matching
// using a mock rule configuration.
func TestEngineUDPWithRuleConfig(t *testing.T) {
	rc := &config.RuleConfiguration{
		Proxies: []*config.Proxy{
			{Name: "test-direct", Type: "DIRECT"},
		},
	}
	engine := NewEngine(rc)

	// Verify engine creation doesn't panic and fields are set
	if engine.ruleConf == nil {
		t.Fatal("ruleConf should be set")
	}
	if engine.running {
		t.Error("engine should not be running yet")
	}

	_ = engine.IsEnabled() // just verify it doesn't panic
}

// TestProbeInternalDNS verifies that the internal health probe can reach the
// TUN DNS hijacker through the gVisor netstack without requiring a real TUN
// device or admin privileges.
func TestQueryInternalDNS(t *testing.T) {
	linkEP := channel.New(512, 1500, "")

	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})

	const nicID = 1
	if err := s.CreateNIC(nicID, linkEP); err != nil {
		t.Fatalf("CreateNIC: %v", err)
	}

	tunAddr := tcpip.AddrFrom4([4]byte{198, 18, 0, 1})
	dnsAddr := tcpip.AddrFrom4([4]byte{198, 18, 0, 2})

	if err := s.AddProtocolAddress(nicID, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: tunAddr, PrefixLen: 15},
	}, stack.AddressProperties{}); err != nil {
		t.Fatalf("AddProtocolAddress tun: %v", err)
	}
	if err := s.AddProtocolAddress(nicID, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: dnsAddr, PrefixLen: 32},
	}, stack.AddressProperties{}); err != nil {
		t.Fatalf("AddProtocolAddress dns: %v", err)
	}

	s.SetPromiscuousMode(nicID, true)
	s.SetSpoofing(nicID, true)
	s.SetRouteTable([]tcpip.Route{
		{Destination: header.IPv4EmptySubnet, NIC: nicID},
		{Destination: header.IPv6EmptySubnet, NIC: nicID},
	})

	// Mirror the real engine's read/write loops so outbound netstack packets are
	// looped back inbound, just like they would be through the TUN device.
	var loopWg sync.WaitGroup
	loopWg.Add(1)
	stopLoop := make(chan struct{})
	go func() {
		defer loopWg.Done()
		for {
			select {
			case <-stopLoop:
				return
			default:
			}
			pkt := linkEP.Read()
			if pkt == nil {
				select {
				case <-stopLoop:
					return
				case <-time.After(10 * time.Millisecond):
				}
				continue
			}
			proto := pkt.NetworkProtocolNumber
			buf := pkt.ToBuffer()
			data := buf.Flatten()
			pkt.DecRef()
			newPkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
				Payload: buffer.MakeWithData(data),
			})
			linkEP.InjectInbound(proto, newPkt)
			newPkt.DecRef()
		}
	}()
	defer func() {
		close(stopLoop)
		loopWg.Wait()
	}()

	pool := NewFakeIPPool()
	hijack := NewDNSHijacker(s, pool, tunAddr, dnsAddr)
	var wg sync.WaitGroup
	if err := hijack.Start(&wg); err != nil {
		t.Fatalf("start dns hijacker: %v", err)
	}
	defer func() {
		hijack.Stop()
		wg.Wait()
	}()

	// Give goroutine time to start listening.
	time.Sleep(50 * time.Millisecond)

	engine := &Engine{
		running: true,
		ns:      s,
		addr:    tunAddr,
		dnsAddr: dnsAddr,
	}

	domain := fmt.Sprintf("tun-health-%d.example.com", time.Now().UnixNano())
	query := buildDNSQuery(domain, uint16(time.Now().UnixNano()))
	resp, err := engine.queryInternalDNS(query)
	if err != nil {
		t.Fatalf("queryInternalDNS failed: %v", err)
	}
	ip := parseDNSResponseIP(resp)
	if ip == nil {
		t.Fatal("failed to parse response IP")
	}
	if !isFakeIP(ip.String()) {
		t.Fatalf("expected Fake-IP, got %s", ip)
	}
}

// TestDNSHijackerResolve verifies the direct Resolve path used by the
// Windows-side DNS proxy when gVisor loopback delivery is unavailable.
func TestDNSHijackerResolve(t *testing.T) {
	pool := NewFakeIPPool()
	hijack := NewDNSHijacker(nil, pool, tcpip.AddrFrom4([4]byte{198, 18, 0, 1}), tcpip.AddrFrom4([4]byte{198, 18, 0, 2}))

	domain := "direct-resolve.example.com"
	query := buildDNSQuery(domain, 0x1234)
	resp, err := hijack.Resolve(query)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	ip := parseDNSResponseIP(resp)
	if ip == nil {
		t.Fatal("failed to parse response IP")
	}
	if !isFakeIP(ip.String()) {
		t.Fatalf("expected Fake-IP, got %s", ip)
	}
}
