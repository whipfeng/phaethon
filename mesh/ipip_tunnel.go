package mesh

import (
	"phaethon/util"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// IPIPProtocolNumber is the IPIP protocol number (IP protocol 4).
const IPIPProtocolNumber tcpip.TransportProtocolNumber = 4

// ipipTunnelProtocol implements stack.TunnelProtocol for IPIP (protocol 4).
// It handles decapsulation of IPIP packets when the outer destination matches
// the local EIP (tunnel terminator).
type ipipTunnelProtocol struct {
	localEIP tcpip.Address
}

// newIPIPTunnelProtocol creates a new IPIP tunnel protocol handler.
func newIPIPTunnelProtocol(localEIP tcpip.Address) stack.TunnelProtocol {
	return &ipipTunnelProtocol{
		localEIP: localEIP,
	}
}

// Number returns the IPIP protocol number (4).
func (p *ipipTunnelProtocol) Number() tcpip.TransportProtocolNumber {
	return IPIPProtocolNumber
}

// ShouldDecapsulate checks if this IPIP packet should be decapsulated.
// Returns true if the outer destination matches our local EIP.
func (p *ipipTunnelProtocol) ShouldDecapsulate(pkt *stack.PacketBuffer) bool {
	util.LogModuleInfo("ipip", "[IPIP-TUNNEL] ShouldDecapsulate called: localEIP=%s (len=%d)", p.localEIP, p.localEIP.Len())
	
	if p.localEIP.Len() == 0 {
		util.LogModuleWarn("ipip", "[IPIP-TUNNEL] ShouldDecapsulate: localEIP is empty")
		return false
	}

	// Get the outer IP header
	if pkt.NetworkProtocolNumber != header.IPv4ProtocolNumber {
		util.LogModuleWarn("ipip", "[IPIP-TUNNEL] ShouldDecapsulate: not IPv4 (proto=%d)", pkt.NetworkProtocolNumber)
		return false
	}

	outerIP := header.IPv4(pkt.NetworkHeader().Slice())
	end := len(outerIP)
	if end > 20 {
		end = 20
	}
	util.LogModuleInfo("ipip", "[IPIP-TUNNEL] ShouldDecapsulate: outerIP len=%d data=%x", len(outerIP), outerIP[:end])
	
	// Skip IsValid() check - gVisor already validated the packet
	// We just need to check the destination address
	if len(outerIP) < 20 {
		util.LogModuleWarn("ipip", "[IPIP-TUNNEL] ShouldDecapsulate: outer IP header too short (len=%d)", len(outerIP))
		return false
	}

	// Check if outer destination matches our local EIP
	dstAddr := outerIP.DestinationAddress()
	shouldDecap := p.localEIP == dstAddr

	util.LogModuleInfo("ipip", "[IPIP-TUNNEL] ShouldDecapsulate: outer dst=%s local EIP=%s match=%v", dstAddr, p.localEIP, shouldDecap)

	return shouldDecap
}

// Decapsulate decapsulates the IPIP packet and returns the inner packet.
func (p *ipipTunnelProtocol) Decapsulate(pkt *stack.PacketBuffer) *stack.PacketBuffer {
	outerIP := header.IPv4(pkt.NetworkHeader().Slice())
	
	// Skip IsValid() check - gVisor already validated the packet
	if len(outerIP) < 20 {
		util.LogModuleWarn("ipip", "[IPIP-TUNNEL] Decapsulate: outer IP header too short (len=%d)", len(outerIP))
		return nil
	}

	// Get inner packet from Data (everything after outer IP header)
	innerBuffer := pkt.Data().ToBuffer()
	defer innerBuffer.Release()
	
	innerData := innerBuffer.Flatten()
	if len(innerData) == 0 {
		util.LogModuleWarn("ipip", "[IPIP-TUNNEL] Decapsulate: empty inner packet")
		return nil
	}

	// Parse inner IP header to validate
	innerIP := header.IPv4(innerData)
	if len(innerIP) < 20 {
		util.LogModuleWarn("ipip", "[IPIP-TUNNEL] Decapsulate: inner IP header too short (len=%d)", len(innerIP))
		return nil
	}

	util.LogModuleInfo("ipip", "[IPIP-TUNNEL] Decapsulating: outer=%s->%s inner=%s->%s proto=%d len=%d",
		outerIP.SourceAddress(), outerIP.DestinationAddress(),
		innerIP.SourceAddress(), innerIP.DestinationAddress(),
		innerIP.TransportProtocol(), len(innerData))

	// Create a new packet buffer for the inner packet
	// The inner packet is a complete IP packet (IP header + transport header + data)
	// It should go through the full HandlePacket flow, just like a normal packet
	innerPkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload:           buffer.MakeWithData(innerData),
		IsForwardedPacket: false,
	})
	innerPkt.NetworkProtocolNumber = header.IPv4ProtocolNumber

	util.LogModuleInfo("ipip", "[IPIP-TUNNEL] Decapsulate: innerData len=%d (complete IP packet, no link header)", len(innerData))

	return innerPkt
}
