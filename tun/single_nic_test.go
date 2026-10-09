package tun

import (
	"bytes"
	"net"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// TestSingleNICDesign verifies the single NIC design with promiscuous mode.
// LocalStack traffic stays inside gVisor; packets that reach linkEP are TUN output.
func TestSingleNICDesign(t *testing.T) {
	const nicID = 1

	dnsAddr := tcpip.AddrFrom4([4]byte{100, 64, 0, 3})
	hostIP := tcpip.AddrFrom4([4]byte{100, 64, 0, 2})
	vip := tcpip.AddrFrom4([4]byte{100, 64, 0, 1})

	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{udp.NewProtocol, tcp.NewProtocol},
	})

	linkEP := channel.New(512, 1500, "tun")
	if err := s.CreateNIC(nicID, linkEP); err != nil {
		t.Fatalf("create NIC: %v", err)
	}

	s.SetRouteSelector(func(dst tcpip.Address) stack.RouteDecision {
		return stack.RouteDecision{
			EgressNIC:  nicID,
			LocalStack: dst == dnsAddr,
			Cacheable:  true,
		}
	})

	ap := tcpip.AddressWithPrefix{Address: dnsAddr, PrefixLen: 32}
	if err := s.AddProtocolAddress(nicID, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: ap,
	}, stack.AddressProperties{}); err != nil {
		t.Fatalf("add dnsAddr: %v", err)
	}

	s.SetPromiscuousMode(nicID, true)
	s.SetSpoofing(nicID, true)
	_ = s.SetForwardingDefaultAndAllNICs(ipv4.ProtocolNumber, true)

	s.SetRouteTable([]tcpip.Route{
		{Destination: header.IPv4EmptySubnet, NIC: nicID},
	})

	// Simulated writeLoop
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	tunWrites := 0
	reinjects := 0
	var tunWriteDst []string
	go func() {
		defer close(doneCh)
		for {
			select {
			case <-stopCh:
				return
			default:
			}
			pkt := linkEP.Read()
			if pkt == nil {
				time.Sleep(5 * time.Millisecond)
				continue
			}
			buf := pkt.ToBuffer()
			data := buf.Flatten()
			pkt.DecRef()

			if len(data) < 20 {
				continue
			}

			dstIP := net.IP(data[16:20])

			tunWrites++
			tunWriteDst = append(tunWriteDst, dstIP.String())
			t.Logf("writeLoop: dst=%s → TUN (writes=%d)", dstIP, tunWrites)
		}
	}()
	defer func() {
		close(stopCh)
		<-doneCh
	}()

	// Test 1: DNS resolution loopback (socket → hijacker → socket)
	t.Run("DNS_loopback", func(t *testing.T) {
		var hijackerWQ waiter.Queue
		hijackerEP, err := s.NewEndpoint(udp.ProtocolNumber, ipv4.ProtocolNumber, &hijackerWQ)
		if err != nil {
			t.Fatalf("create hijacker endpoint: %v", err)
		}
		defer hijackerEP.Close()

		if err := hijackerEP.Bind(tcpip.FullAddress{Addr: dnsAddr, Port: 53}); err != nil {
			t.Fatalf("hijacker bind: %v", err)
		}

		hijackerWaitEntry, hijackerCh := waiter.NewChannelEntry(waiter.EventIn)
		hijackerWQ.EventRegister(&hijackerWaitEntry)
		defer hijackerWQ.EventUnregister(&hijackerWaitEntry)

		var wq waiter.Queue
		ep, err := s.NewEndpoint(udp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
		if err != nil {
			t.Fatalf("create UDP endpoint: %v", err)
		}
		defer ep.Close()

		if err := ep.Bind(tcpip.FullAddress{}); err != nil {
			t.Fatalf("bind: %v", err)
		}
		if err := ep.Connect(tcpip.FullAddress{Addr: dnsAddr, Port: 53}); err != nil {
			t.Fatalf("connect: %v", err)
		}

		waitEntry, ch := waiter.NewChannelEntry(waiter.EventIn)
		wq.EventRegister(&waitEntry)
		defer wq.EventUnregister(&waitEntry)

		query := []byte{0x00, 0x01, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x07, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 0x03, 'c', 'o', 'm', 0x00,
			0x00, 0x01, 0x00, 0x01}
		if _, err := ep.Write(&slicePayload{data: query}, tcpip.WriteOptions{}); err != nil {
			t.Fatalf("write: %v", err)
		}

		select {
		case <-hijackerCh:
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout waiting for hijacker")
		}

		var hijackerBuf bytes.Buffer
		res, err := hijackerEP.Read(&hijackerBuf, tcpip.ReadOptions{NeedRemoteAddr: true})
		if err != nil {
			t.Fatalf("hijacker read: %v", err)
		}
		t.Logf("hijacker received query from %s", res.RemoteAddr.Addr)

		resp := []byte{0x00, 0x01, 0x81, 0x80, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00}
		resp = append(resp, 0x07, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 0x03, 'c', 'o', 'm', 0x00)
		resp = append(resp, 0x00, 0x01, 0x00, 0x01)
		resp = append(resp, 0xc0, 0x0c)
		resp = append(resp, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x00, 0x3c, 0x00, 0x04)
		resp = append(resp, 198, 18, 0, 1)

		if _, err := hijackerEP.Write(&slicePayload{data: resp}, tcpip.WriteOptions{To: &res.RemoteAddr}); err != nil {
			t.Fatalf("hijacker write: %v", err)
		}

		select {
		case <-ch:
			var buf bytes.Buffer
			if _, err := ep.Read(&buf, tcpip.ReadOptions{}); err != nil {
				t.Fatalf("socket read: %v", err)
			}
			fakeIPResp, _ := parseDNSResponseIP(buf.Bytes())
			if fakeIPResp == nil {
				t.Fatalf("failed to parse DNS response")
			}
			t.Logf("socket received DNS response: %s", fakeIPResp)
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout waiting for DNS response")
		}
	})

	// Test 2: Forwarder response to VIP reaches linkEP outbound
	// Simulates: forwarder socket writes data back to VIP (bypass gateway client)
	t.Run("Forwarder_response_to_VIP", func(t *testing.T) {
		prevTunWrites := tunWrites

		// Create a UDP socket that writes to VIP (simulating forwarder response)
		var wq waiter.Queue
		ep, err := s.NewEndpoint(udp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
		if err != nil {
			t.Fatalf("create endpoint: %v", err)
		}
		defer ep.Close()

		if err := ep.Bind(tcpip.FullAddress{}); err != nil {
			t.Fatalf("bind: %v", err)
		}

		// Write to VIP (simulating forwarder writing response back to bypass gateway client)
		payload := []byte{0x00, 0x01, 0x02, 0x03}
		remoteAddr := tcpip.FullAddress{Addr: vip, Port: 12345}
		if _, err := ep.Write(&slicePayload{data: payload}, tcpip.WriteOptions{To: &remoteAddr}); err != nil {
			t.Fatalf("write to VIP: %v", err)
		}

		// Wait for writeLoop to process
		time.Sleep(300 * time.Millisecond)

		if tunWrites <= prevTunWrites {
			t.Errorf("FAIL: forwarder response to VIP did NOT reach writeLoop TUN path (tunWrites=%d, prev=%d)", tunWrites, prevTunWrites)
		} else {
			t.Logf("PASS: forwarder response to VIP correctly written to TUN (tunWrites=%d)", tunWrites)
		}
	})

	// Test 3: Forwarder response to hostIP reaches linkEP outbound
	t.Run("Forwarder_response_to_hostIP", func(t *testing.T) {
		prevTunWrites := tunWrites

		var wq waiter.Queue
		ep, err := s.NewEndpoint(udp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
		if err != nil {
			t.Fatalf("create endpoint: %v", err)
		}
		defer ep.Close()

		if err := ep.Bind(tcpip.FullAddress{}); err != nil {
			t.Fatalf("bind: %v", err)
		}

		payload := []byte{0x00, 0x01, 0x02, 0x03}
		remoteAddr := tcpip.FullAddress{Addr: hostIP, Port: 12345}
		if _, err := ep.Write(&slicePayload{data: payload}, tcpip.WriteOptions{To: &remoteAddr}); err != nil {
			t.Fatalf("write to hostIP: %v", err)
		}

		time.Sleep(300 * time.Millisecond)

		if tunWrites <= prevTunWrites {
			t.Errorf("FAIL: response to hostIP did NOT reach writeLoop TUN path")
		} else {
			t.Logf("PASS: response to hostIP correctly written to TUN (tunWrites=%d)", tunWrites)
		}
	})

	// Test 4: conntrack restores an arbitrary client destination and records
	// the original TUN NIC. That per-flow route must override Branch 4's
	// destination-only LocalStack policy.
	t.Run("Conntrack_return_uses_output_NIC", func(t *testing.T) {
		prevTunWrites := tunWrites
		clientAddr := tcpip.AddrFrom4([4]byte{192, 0, 2, 10})
		udpHdr := make([]byte, header.UDPMinimumSize)
		header.UDP(udpHdr).Encode(&header.UDPFields{
			SrcPort: 443,
			DstPort: 54321,
			Length:  header.UDPMinimumSize,
		})
		ipBuf := make([]byte, header.IPv4MinimumSize+len(udpHdr))
		copy(ipBuf[header.IPv4MinimumSize:], udpHdr)
		ip := header.IPv4(ipBuf)
		ip.Encode(&header.IPv4Fields{
			TotalLength: uint16(len(ipBuf)),
			TTL:         64,
			Protocol:    uint8(udp.ProtocolNumber),
			SrcAddr:     dnsAddr,
			DstAddr:     clientAddr,
		})
		ip.SetChecksum(^ip.CalculateChecksum())

		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
			Payload: buffer.MakeWithData(ipBuf),
		})
		pkt.OutputNICName = "tun"
		linkEP.InjectInbound(ipv4.ProtocolNumber, pkt)
		pkt.DecRef()

		deadline := time.After(2 * time.Second)
		for tunWrites == prevTunWrites {
			select {
			case <-deadline:
				t.Fatal("conntrack return was not emitted through TUN")
			default:
				time.Sleep(5 * time.Millisecond)
			}
		}
		if got := tunWriteDst[len(tunWriteDst)-1]; got != clientAddr.String() {
			t.Fatalf("conntrack return TUN destination = %s, want %s", got, clientAddr)
		}
	})

	t.Logf("Final stats: tunWrites=%d, reinjects=%d, tunWriteDst=%v", tunWrites, reinjects, tunWriteDst)
}
