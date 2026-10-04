package mesh

import (
	"phaethon/util"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/waiter"
)

// IPIPProtocolNumber is the IPIP protocol number (IP protocol 4).
const IPIPProtocolNumber tcpip.TransportProtocolNumber = 4

// ipipProtocol implements stack.TransportProtocol for IPIP (protocol 4).
// It handles decapsulation of IPIP packets: strips the outer IP header and
// injects the inner packet into NIC 1 (TUN) for further processing.
type ipipProtocol struct {
	stack  *stack.Stack
	linkEP *channel.Endpoint // NIC 1 (TUN) for re-injection
}

// Number returns the IPIP protocol number (4).
func (p *ipipProtocol) Number() tcpip.TransportProtocolNumber {
	return IPIPProtocolNumber
}

// NewEndpoint is not supported for IPIP (no sockets).
func (p *ipipProtocol) NewEndpoint(netProto tcpip.NetworkProtocolNumber, waitQueue *waiter.Queue) (tcpip.Endpoint, tcpip.Error) {
	return nil, &tcpip.ErrNotSupported{}
}

// NewRawEndpoint is not supported for IPIP.
func (p *ipipProtocol) NewRawEndpoint(netProto tcpip.NetworkProtocolNumber, waitQueue *waiter.Queue) (tcpip.Endpoint, tcpip.Error) {
	return nil, &tcpip.ErrNotSupported{}
}

// MinimumPacketSize returns the minimum IPIP packet size (outer IP header only).
func (p *ipipProtocol) MinimumPacketSize() int {
	return header.IPv4MinimumSize
}

// ParsePorts returns fake ports for IPIP (no actual ports).
func (p *ipipProtocol) ParsePorts(b []byte) (src, dst uint16, err tcpip.Error) {
	// IPIP has no ports; return zeros
	return 0, 0, nil
}

// HandleUnknownDestinationPacket handles IPIP packets by decapsulating them
// and injecting the inner packet into NIC 1 (TUN).
func (p *ipipProtocol) HandleUnknownDestinationPacket(id stack.TransportEndpointID, pkt *stack.PacketBuffer) stack.UnknownDestinationPacketDisposition {
	// Get the outer IP header
	if pkt.NetworkProtocolNumber != header.IPv4ProtocolNumber {
		util.LogWarn("[IPIP] non-IPv4 outer packet, dropping")
		return stack.UnknownDestinationPacketHandled
	}

	// Extract outer IPv4 header
	outerIP := header.IPv4(pkt.NetworkHeader().Slice())
	if !outerIP.IsValid(len(outerIP)) {
		util.LogWarn("[IPIP] invalid outer IP header")
		return stack.UnknownDestinationPacketHandled
	}

	// Get inner packet (everything after outer IP header)
	outerHdrLen := int(outerIP.HeaderLength())
	innerData := pkt.NetworkHeader().Slice()[outerHdrLen:]
	if len(innerData) == 0 {
		util.LogWarn("[IPIP] empty inner packet")
		return stack.UnknownDestinationPacketHandled
	}

	// Parse inner IP header to validate
	innerIP := header.IPv4(innerData)
	if !innerIP.IsValid(len(innerIP)) {
		util.LogWarn("[IPIP] invalid inner IP header")
		return stack.UnknownDestinationPacketHandled
	}

	util.LogDebug("[IPIP] decapsulating: outer=%s -> %s, inner=%s -> %s proto=%d",
		outerIP.SourceAddress(), outerIP.DestinationAddress(),
		innerIP.SourceAddress(), innerIP.DestinationAddress(),
		innerIP.TransportProtocol())

	// Create a new packet buffer for the inner packet
	innerPkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload:           buffer.MakeWithData(innerData),
		IsForwardedPacket: false,
	})

	// Inject inner packet into NIC 1 (TUN) in promiscuous mode
	// This allows transit traffic (dst=internet/mesh) to be accepted
	if p.linkEP == nil {
		util.LogWarn("[IPIP] linkEP is nil")
		innerPkt.DecRef()
		return stack.UnknownDestinationPacketHandled
	}

	// Inject the inner packet
	p.linkEP.InjectInbound(header.IPv4ProtocolNumber, innerPkt)
	innerPkt.DecRef()

	return stack.UnknownDestinationPacketHandled
}

// Parse sets the transport header for IPIP packets.
// Since IPIP doesn't have a transport header, we just return true.
func (p *ipipProtocol) Parse(pkt *stack.PacketBuffer) bool {
	// IPIP doesn't have a traditional transport header
	// The inner IP packet is the "payload"
	return true
}

// SetOption is not supported.
func (p *ipipProtocol) SetOption(option tcpip.SettableTransportProtocolOption) tcpip.Error {
	return &tcpip.ErrNotSupported{}
}

// Option is not supported.
func (p *ipipProtocol) Option(option tcpip.GettableTransportProtocolOption) tcpip.Error {
	return &tcpip.ErrNotSupported{}
}

// Close is a no-op for IPIP.
func (p *ipipProtocol) Close() {}

// Wait is a no-op for IPIP.
func (p *ipipProtocol) Wait() {}

// Pause is a no-op for IPIP.
func (p *ipipProtocol) Pause() {}

// Resume is a no-op for IPIP.
func (p *ipipProtocol) Resume() {}

// Restore is a no-op for IPIP.
func (p *ipipProtocol) Restore() {}

// newIPIPProtocol creates a new IPIP protocol handler.
func newIPIPProtocol(s *stack.Stack, linkEP *channel.Endpoint) stack.TransportProtocol {
	return &ipipProtocol{
		stack:  s,
		linkEP: linkEP,
	}
}
