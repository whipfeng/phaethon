package mesh

import (
	"net"
	"testing"
)

// buildTestPacket builds a minimal TCP or UDP packet for testing.
func buildTestPacket(proto byte, srcIP, dstIP net.IP, srcPort, dstPort uint16) []byte {
	srcIP4 := srcIP.To4()
	dstIP4 := dstIP.To4()

	// IPv4 header (20 bytes) + TCP/UDP header (8 bytes)
	pkt := make([]byte, 28)

	// IPv4 header
	pkt[0] = 0x45                         // version=4, IHL=5
	pkt[1] = 0                            // DSCP/ECN
	pkt[2] = 0                            // total length (high)
	pkt[3] = 28                           // total length (low) = 28
	pkt[4] = 0                            // identification
	pkt[5] = 0                            // flags + fragment offset
	pkt[6] = 0                            // fragment offset
	pkt[7] = 0                            // TTL
	pkt[8] = 0x40                         // TTL = 64
	pkt[9] = proto                        // protocol (6=TCP, 17=UDP)
	pkt[10] = 0                           // checksum (high)
	pkt[11] = 0                           // checksum (low)
	copy(pkt[12:16], srcIP4)              // source IP
	copy(pkt[16:20], dstIP4)              // destination IP

	// TCP/UDP header
	pkt[20] = byte(srcPort >> 8)          // source port (high)
	pkt[21] = byte(srcPort)               // source port (low)
	pkt[22] = byte(dstPort >> 8)          // destination port (high)
	pkt[23] = byte(dstPort)               // destination port (low)

	return pkt
}

func TestNATTable_OutboundInbound(t *testing.T) {
	vip := net.ParseIP("100.64.0.1").To4()
	nat := NewNATTable(vip)
	defer nat.Stop()

	// Original packet: 192.168.1.100:12345 -> 8.8.8.8:443 (TCP)
	origSrc := net.ParseIP("192.168.1.100").To4()
	dst := net.ParseIP("8.8.8.8").To4()
	pkt := buildTestPacket(6, origSrc, dst, 12345, 443)

	// Translate outbound
	outPkt := nat.TranslateOutbound(pkt)
	if outPkt == nil {
		t.Fatal("TranslateOutbound returned nil")
	}

	// Check source IP is now VIP
	outSrcIP := net.IP(outPkt[12:16])
	if !outSrcIP.Equal(vip) {
		t.Errorf("outbound src IP = %s, want %s", outSrcIP, vip)
	}

	// Check source port is mapped (not 12345)
	outSrcPort := uint16(outPkt[20])<<8 | uint16(outPkt[21])
	if outSrcPort == 12345 {
		t.Errorf("outbound src port should be mapped, got %d", outSrcPort)
	}

	// Check destination unchanged
	outDstIP := net.IP(outPkt[16:20])
	if !outDstIP.Equal(dst) {
		t.Errorf("outbound dst IP = %s, want %s", outDstIP, dst)
	}

	// Simulate response: 8.8.8.8:443 -> 100.64.0.1:<mappedPort> (TCP)
	respPkt := buildTestPacket(6, dst, vip, 443, outSrcPort)

	// Translate inbound
	inPkt := nat.TranslateInbound(respPkt)
	if inPkt == nil {
		t.Fatal("TranslateInbound returned nil")
	}

	// Check destination IP is restored to original
	inDstIP := net.IP(inPkt[16:20])
	if !inDstIP.Equal(origSrc) {
		t.Errorf("inbound dst IP = %s, want %s", inDstIP, origSrc)
	}

	// Check destination port is restored
	inDstPort := uint16(inPkt[22])<<8 | uint16(inPkt[23])
	if inDstPort != 12345 {
		t.Errorf("inbound dst port = %d, want 12345", inDstPort)
	}
}

func TestNATTable_UDP(t *testing.T) {
	vip := net.ParseIP("100.64.0.1").To4()
	nat := NewNATTable(vip)
	defer nat.Stop()

	// UDP packet: 192.168.1.50:5000 -> 1.1.1.1:53
	origSrc := net.ParseIP("192.168.1.50").To4()
	dst := net.ParseIP("1.1.1.1").To4()
	pkt := buildTestPacket(17, origSrc, dst, 5000, 53)

	outPkt := nat.TranslateOutbound(pkt)
	if outPkt == nil {
		t.Fatal("TranslateOutbound returned nil for UDP")
	}

	outSrcPort := uint16(outPkt[20])<<8 | uint16(outPkt[21])
	respPkt := buildTestPacket(17, dst, vip, 53, outSrcPort)

	inPkt := nat.TranslateInbound(respPkt)
	if inPkt == nil {
		t.Fatal("TranslateInbound returned nil for UDP response")
	}

	inDstIP := net.IP(inPkt[16:20])
	if !inDstIP.Equal(origSrc) {
		t.Errorf("UDP inbound dst IP = %s, want %s", inDstIP, origSrc)
	}
}

func TestNATTable_NoMapping(t *testing.T) {
	vip := net.ParseIP("100.64.0.1").To4()
	nat := NewNATTable(vip)
	defer nat.Stop()

	// Response without prior outbound should return nil
	dst := net.ParseIP("8.8.8.8").To4()
	respPkt := buildTestPacket(6, dst, vip, 443, 54321)

	inPkt := nat.TranslateInbound(respPkt)
	if inPkt != nil {
		t.Error("TranslateInbound should return nil for unknown mapping")
	}
}

func TestNATTable_MultipleConnections(t *testing.T) {
	vip := net.ParseIP("100.64.0.1").To4()
	nat := NewNATTable(vip)
	defer nat.Stop()

	// Two different source IPs connecting to same destination
	src1 := net.ParseIP("192.168.1.100").To4()
	src2 := net.ParseIP("192.168.1.101").To4()
	dst := net.ParseIP("8.8.8.8").To4()

	pkt1 := buildTestPacket(6, src1, dst, 11111, 443)
	pkt2 := buildTestPacket(6, src2, dst, 22222, 443)

	out1 := nat.TranslateOutbound(pkt1)
	out2 := nat.TranslateOutbound(pkt2)

	if out1 == nil || out2 == nil {
		t.Fatal("TranslateOutbound failed")
	}

	// Each should have different mapped ports
	port1 := uint16(out1[20])<<8 | uint16(out1[21])
	port2 := uint16(out2[20])<<8 | uint16(out2[21])
	if port1 == port2 {
		t.Errorf("different connections should have different mapped ports")
	}

	// Responses should route back correctly
	resp1 := buildTestPacket(6, dst, vip, 443, port1)
	resp2 := buildTestPacket(6, dst, vip, 443, port2)

	in1 := nat.TranslateInbound(resp1)
	in2 := nat.TranslateInbound(resp2)

	dst1 := net.IP(in1[16:20])
	dst2 := net.IP(in2[16:20])

	if !dst1.Equal(src1) {
		t.Errorf("response 1 dst = %s, want %s", dst1, src1)
	}
	if !dst2.Equal(src2) {
		t.Errorf("response 2 dst = %s, want %s", dst2, src2)
	}
}
