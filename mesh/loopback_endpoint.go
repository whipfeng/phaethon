package mesh

import (
	"sync"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// LoopbackEndpoint implements a loopback link endpoint for NIC 3.
// It receives outbound packets and either:
// 1. IPIP encapsulates them (for non-mesh traffic matching static routes)
// 2. Loops them back to inbound (for local delivery)
//
// This endpoint is used in the multi-NIC architecture to handle IPIP
// encapsulation and loopback at the gVisor netstack level.
type LoopbackEndpoint struct {
	mu sync.RWMutex

	// dispatcher is the network dispatcher for delivering packets to the netstack
	dispatcher stack.NetworkDispatcher

	// mtu is the maximum transmission unit
	mtu uint32

	// attached indicates if the endpoint is attached to a dispatcher
	attached bool

	// stats for monitoring
	stats LoopbackStats
}

// LoopbackStats tracks loopback endpoint statistics
type LoopbackStats struct {
	PacketsReceived  uint64
	PacketsLooped    uint64
	PacketsEncapsulated uint64
	BytesReceived    uint64
	BytesLooped      uint64
}

// NewLoopbackEndpoint creates a new loopback endpoint
func NewLoopbackEndpoint(mtu uint32) *LoopbackEndpoint {
	return &LoopbackEndpoint{
		mtu: mtu,
	}
}

// Attach saves the network dispatcher for delivering packets to the netstack
func (e *LoopbackEndpoint) Attach(dispatcher stack.NetworkDispatcher) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.dispatcher = dispatcher
	e.attached = true
}

// IsAttached returns whether the endpoint is attached to a dispatcher
func (e *LoopbackEndpoint) IsAttached() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.attached
}

// MTU returns the maximum transmission unit
func (e *LoopbackEndpoint) MTU() uint32 {
	return e.mtu
}

// Capabilities returns the link endpoint capabilities
func (e *LoopbackEndpoint) Capabilities() stack.LinkEndpointCapabilities {
	return 0
}

// MaxHeaderLength returns the maximum header length
func (e *LoopbackEndpoint) MaxHeaderLength() uint16 {
	return 0
}

// LinkAddress returns the link layer address
func (e *LoopbackEndpoint) LinkAddress() tcpip.LinkAddress {
	return ""
}

// WritePackets is called by the netstack to send packets.
// For loopback endpoint, we loop the packets back to the inbound path.
func (e *LoopbackEndpoint) WritePackets(pkts stack.PacketBufferList) (int, tcpip.Error) {
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

		// TODO: Phase 3 - Add IPIP encapsulation logic here
		// For now, just loop back all packets

		// Increment reference count since DeliverNetworkPacket is async
		pkt.IncRef()
		
		// Loop back to inbound path
		dispatcher.DeliverNetworkPacket(pkt.NetworkProtocolNumber, pkt)
		
		e.stats.PacketsLooped++
		e.stats.BytesLooped += uint64(pkt.Size())
		count++
	}

	return count, nil
}

// Wait implements stack.LinkEndpoint.Wait
func (e *LoopbackEndpoint) Wait() {}

// ARPHardwareType implements stack.LinkEndpoint.ARPHardwareType
func (e *LoopbackEndpoint) ARPHardwareType() header.ARPHardwareType {
	return header.ARPHardwareNone
}

// AddHeader implements stack.LinkEndpoint.AddHeader
func (e *LoopbackEndpoint) AddHeader(pkt *stack.PacketBuffer) {}

// ParseHeader implements stack.LinkEndpoint.ParseHeader
func (e *LoopbackEndpoint) ParseHeader(pkt *stack.PacketBuffer) bool {
	return true
}

// Close implements stack.LinkEndpoint.Close
func (e *LoopbackEndpoint) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.attached = false
	e.dispatcher = nil
}

// Stats returns the loopback endpoint statistics
func (e *LoopbackEndpoint) Stats() LoopbackStats {
	return e.stats
}
