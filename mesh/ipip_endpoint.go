package mesh

import (
	"net"
	"sync"

	"phaethon/util"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// IPIPEndpoint implements a dedicated IPIP decapsulation endpoint for NIC 3.
// Per multi_nic_architecture_v2 design, NIC 3 only handles IPIP decapsulation.
// It receives IPIP packets (outer dst=local EIP), decapsulates them, and loops back to Forwarder.
type IPIPEndpoint struct {
	mu sync.RWMutex

	// dispatcher is the network dispatcher for delivering packets to the netstack
	dispatcher stack.NetworkDispatcher

	// mtu is the maximum transmission unit
	mtu uint32

	// attached indicates if the endpoint is attached to a dispatcher
	attached bool

	// stats for monitoring
	stats IPIPStats
}

// IPIPStats tracks IPIP endpoint statistics
type IPIPStats struct {
	PacketsReceived   uint64
	PacketsDecapsulated uint64
	BytesReceived     uint64
	BytesDecapsulated uint64
	DecapsulationErrors uint64
}

// NewIPIPEndpoint creates a new IPIP endpoint
func NewIPIPEndpoint(mtu uint32) *IPIPEndpoint {
	return &IPIPEndpoint{
		mtu: mtu,
	}
}

// Attach saves the network dispatcher for delivering packets to the netstack
func (e *IPIPEndpoint) Attach(dispatcher stack.NetworkDispatcher) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.dispatcher = dispatcher
	e.attached = true
}

// IsAttached returns whether the endpoint is attached to a dispatcher
func (e *IPIPEndpoint) IsAttached() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.attached
}

// MTU returns the maximum transmission unit
func (e *IPIPEndpoint) MTU() uint32 {
	return e.mtu
}

// SetMTU sets the maximum transmission unit
func (e *IPIPEndpoint) SetMTU(mtu uint32) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.mtu = mtu
}

// Capabilities returns the link endpoint capabilities
func (e *IPIPEndpoint) Capabilities() stack.LinkEndpointCapabilities {
	return 0
}

// MaxHeaderLength returns the maximum header length
func (e *IPIPEndpoint) MaxHeaderLength() uint16 {
	return 0
}

// LinkAddress returns the link layer address
func (e *IPIPEndpoint) LinkAddress() tcpip.LinkAddress {
	return ""
}

// SetLinkAddress sets the link layer address
func (e *IPIPEndpoint) SetLinkAddress(addr tcpip.LinkAddress) {
	// IPIP endpoint doesn't use link addresses
}

// WritePackets is called by the netstack to send packets.
// For IPIP endpoint, this handles IPIP decapsulation.
func (e *IPIPEndpoint) WritePackets(pkts stack.PacketBufferList) (int, tcpip.Error) {
	e.mu.RLock()
	dispatcher := e.dispatcher
	e.mu.RUnlock()

	if dispatcher == nil {
		return 0, &tcpip.ErrInvalidEndpointState{}
	}

	count := 0
	for _, pkt := range pkts.AsSlice() {
		e.stats.PacketsReceived++
		e.stats.BytesReceived += uint64(pkt.Size())

		// Extract packet data
		buf := pkt.ToBuffer()
		data := buf.Flatten()

		if len(data) < 20 {
			util.LogWarn("[IPIP-EP] packet too short: %d bytes", len(data))
			continue
		}

		srcIP := net.IP(data[12:16])
		dstIP := net.IP(data[16:20])
		proto := data[9]

		if e.stats.PacketsReceived <= 10 {
			util.LogDebug("[IPIP-EP] packet #%d: src=%v dst=%v proto=%d",
				e.stats.PacketsReceived, srcIP, dstIP, proto)
		}

		// IPIP endpoint only handles IPIP packets (proto=4)
		if proto != 4 {
			util.LogWarn("[IPIP-EP] non-IPIP packet received (proto=%d), dropping", proto)
			continue
		}

		// Decapsulate IPIP packet
		innerPacket, err := Decapsulate(data)
		if err != nil {
			util.LogWarn("[IPIP-EP] decapsulation failed: %v", err)
			e.stats.DecapsulationErrors++
			continue
		}
		if innerPacket == nil {
			util.LogWarn("[IPIP-EP] decapsulation returned nil")
			e.stats.DecapsulationErrors++
			continue
		}

		// Loopback decapsulated packet to Forwarder
		newPkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
			Payload: buffer.MakeWithData(innerPacket),
		})
		dispatcher.DeliverNetworkPacket(pkt.NetworkProtocolNumber, newPkt)
		newPkt.DecRef()

		e.stats.PacketsDecapsulated++
		e.stats.BytesDecapsulated += uint64(len(innerPacket))
		count++

		util.LogDebug("[IPIP-EP] decapsulated packet from %v, inner dst=%v",
			srcIP, net.IP(innerPacket[16:20]))
	}

	return count, nil
}

// Wait implements stack.LinkEndpoint.Wait
func (e *IPIPEndpoint) Wait() {}

// ARPHardwareType implements stack.LinkEndpoint.ARPHardwareType
func (e *IPIPEndpoint) ARPHardwareType() header.ARPHardwareType {
	return header.ARPHardwareNone
}

// AddHeader implements stack.LinkEndpoint.AddHeader
func (e *IPIPEndpoint) AddHeader(pkt *stack.PacketBuffer) {}

// ParseHeader implements stack.LinkEndpoint.ParseHeader
func (e *IPIPEndpoint) ParseHeader(pkt *stack.PacketBuffer) bool {
	return true
}

// Close implements stack.LinkEndpoint.Close
func (e *IPIPEndpoint) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.attached = false
	e.dispatcher = nil
}

// SetOnCloseAction sets the action that will be executed before closing the endpoint
func (e *IPIPEndpoint) SetOnCloseAction(func()) {
	// IPIP endpoint doesn't need a close action
}

// Stats returns the IPIP endpoint statistics
func (e *IPIPEndpoint) Stats() IPIPStats {
	return e.stats
}
