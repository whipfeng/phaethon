package mesh

import (
	"sync"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// MeshEndpoint implements a mesh link endpoint for NIC 2.
// It handles mesh traffic (VIP and mesh subnet) and is bound to GIP.
//
// This endpoint is used in the multi-NIC architecture to separate mesh traffic
// from TUN traffic and default traffic.
type MeshEndpoint struct {
	mu sync.RWMutex

	// dispatcher is the network dispatcher for delivering packets to the netstack
	dispatcher stack.NetworkDispatcher

	// mtu is the maximum transmission unit
	mtu uint32

	// attached indicates if the endpoint is attached to a dispatcher
	attached bool

	// stats for monitoring
	stats MeshEndpointStats

	// meshManager is used to send packets via mesh P2P links
	meshManager *MeshManager
}

// MeshEndpointStats tracks mesh endpoint statistics
type MeshEndpointStats struct {
	PacketsReceived  uint64
	PacketsSent      uint64
	BytesReceived    uint64
	BytesSent        uint64
}

// NewMeshEndpoint creates a new mesh endpoint
func NewMeshEndpoint(mtu uint32) *MeshEndpoint {
	return &MeshEndpoint{
		mtu: mtu,
	}
}

// SetMeshManager sets the mesh manager for sending packets
func (e *MeshEndpoint) SetMeshManager(meshMgr *MeshManager) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.meshManager = meshMgr
}

// Attach saves the network dispatcher for delivering packets to the netstack
func (e *MeshEndpoint) Attach(dispatcher stack.NetworkDispatcher) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.dispatcher = dispatcher
	e.attached = true
}

// IsAttached returns whether the endpoint is attached to a dispatcher
func (e *MeshEndpoint) IsAttached() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.attached
}

// MTU returns the maximum transmission unit
func (e *MeshEndpoint) MTU() uint32 {
	return e.mtu
}

// SetMTU sets the maximum transmission unit
func (e *MeshEndpoint) SetMTU(mtu uint32) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.mtu = mtu
}

// Capabilities returns the link endpoint capabilities
func (e *MeshEndpoint) Capabilities() stack.LinkEndpointCapabilities {
	return stack.CapabilityResolutionRequired
}

// MaxHeaderLength returns the maximum header length
func (e *MeshEndpoint) MaxHeaderLength() uint16 {
	return 0
}

// LinkAddress returns the link layer address
func (e *MeshEndpoint) LinkAddress() tcpip.LinkAddress {
	return ""
}

// SetLinkAddress sets the link layer address
func (e *MeshEndpoint) SetLinkAddress(addr tcpip.LinkAddress) {
	// Mesh endpoint doesn't use link addresses
}

// WritePackets is called by the netstack to send packets via mesh P2P links
func (e *MeshEndpoint) WritePackets(pkts stack.PacketBufferList) (int, tcpip.Error) {
	e.mu.RLock()
	meshMgr := e.meshManager
	e.mu.RUnlock()

	if meshMgr == nil {
		return 0, &tcpip.ErrInvalidEndpointState{}
	}

	count := 0
	for _, pkt := range pkts.AsSlice() {
		e.stats.PacketsSent++
		e.stats.BytesSent += uint64(pkt.Size())

		// Get the raw packet data
		buf := pkt.ToBuffer()
		data := buf.Flatten()

		// Send via mesh P2P links
		// The mesh manager will handle routing to the correct peer
		if err := meshMgr.SendRawPacket(data); err != nil {
			// Log error but continue processing other packets
			continue
		}

		count++
	}

	return count, nil
}

// DeliverNetworkPacket delivers a received mesh packet to the netstack
func (e *MeshEndpoint) DeliverNetworkPacket(data []byte) {
	e.mu.RLock()
	dispatcher := e.dispatcher
	e.mu.RUnlock()

	if dispatcher == nil || len(data) == 0 {
		return
	}

	e.stats.PacketsReceived++
	e.stats.BytesReceived += uint64(len(data))

	// Determine protocol version
	var proto tcpip.NetworkProtocolNumber
	if len(data) > 0 {
		version := data[0] >> 4
		if version == 4 {
			proto = header.IPv4ProtocolNumber
		} else if version == 6 {
			proto = header.IPv6ProtocolNumber
		} else {
			return // Invalid IP version
		}
	}

	// Create packet buffer and deliver to netstack
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(data),
	})
	dispatcher.DeliverNetworkPacket(proto, pkt)
	pkt.DecRef()
}

// Wait implements stack.LinkEndpoint.Wait
func (e *MeshEndpoint) Wait() {}

// ARPHardwareType implements stack.LinkEndpoint.ARPHardwareType
func (e *MeshEndpoint) ARPHardwareType() header.ARPHardwareType {
	return header.ARPHardwareNone
}

// AddHeader implements stack.LinkEndpoint.AddHeader
func (e *MeshEndpoint) AddHeader(pkt *stack.PacketBuffer) {}

// ParseHeader implements stack.LinkEndpoint.ParseHeader
func (e *MeshEndpoint) ParseHeader(pkt *stack.PacketBuffer) bool {
	return true
}

// Close implements stack.LinkEndpoint.Close
func (e *MeshEndpoint) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.attached = false
	e.dispatcher = nil
}

// SetOnCloseAction sets the action that will be executed before closing the endpoint
func (e *MeshEndpoint) SetOnCloseAction(func()) {
	// Mesh endpoint doesn't need a close action
}

// Stats returns the mesh endpoint statistics
func (e *MeshEndpoint) Stats() MeshEndpointStats {
	return e.stats
}
