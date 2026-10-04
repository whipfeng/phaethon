package mesh

import (
	"net"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// LinkNIC is a "dumb" NIC for direct mesh peer communication.
// It only handles sending packets to the associated peer.
// No IPIP encapsulation, no routing decisions - just link-layer sending.
type LinkNIC struct {
	nicID      tcpip.NICID
	peerNodeID string
	peerSubnet *net.IPNet // Peer's subnet for routing
	mtu        uint32
	meshMgr    *MeshManager
	
	// Link endpoint interface
	ep stack.LinkEndpoint
}

// NewLinkNIC creates a new LinkNIC for a direct mesh peer.
func NewLinkNIC(nicID tcpip.NICID, peerNodeID string, peerSubnet *net.IPNet, meshMgr *MeshManager) *LinkNIC {
	return &LinkNIC{
		nicID:      nicID,
		peerNodeID: peerNodeID,
		peerSubnet: peerSubnet,
		mtu:        1500, // Default MTU
		meshMgr:    meshMgr,
	}
}

// Attach implements stack.LinkEndpoint.Attach.
func (l *LinkNIC) Attach(dispatcher stack.NetworkDispatcher) {
	// LinkNIC doesn't receive packets directly - mesh endpoint handles reception
	// This is a "send-only" NIC for direct peer communication
}

// IsAttached implements stack.LinkEndpoint.IsAttached.
func (l *LinkNIC) IsAttached() bool {
	return false // LinkNIC is send-only
}

// MTU implements stack.LinkEndpoint.MTU.
func (l *LinkNIC) MTU() uint32 {
	return l.mtu
}

// Capabilities implements stack.LinkEndpoint.Capabilities.
func (l *LinkNIC) Capabilities() stack.LinkEndpointCapabilities {
	return stack.CapabilityNone
}

// MaxHeaderLength implements stack.LinkEndpoint.MaxHeaderLength.
func (l *LinkNIC) MaxHeaderLength() uint16 {
	return 0 // No link-layer header for mesh P2P
}

// LinkAddress implements stack.LinkEndpoint.LinkAddress.
func (l *LinkNIC) LinkAddress() tcpip.LinkAddress {
	return "" // No link-layer address for mesh P2P
}

// WritePackets implements stack.LinkEndpoint.WritePackets.
// Sends packets to the direct mesh peer via mesh P2P network.
func (l *LinkNIC) WritePackets(pkts stack.PacketBufferList) (int, tcpip.Error) {
	if l.meshMgr == nil {
		return 0, &tcpip.ErrInvalidEndpointState{}
	}

	count := 0
	for _, pkt := range pkts.AsSlice() {
		// Get the packet data
		pktData := pkt.ToBuffer().Flatten()
		
		// Send via mesh P2P to the direct peer
		err := l.meshMgr.SendToPeer(l.peerNodeID, pktData)
		if err != nil {
			// Log error but continue with other packets
			continue
		}
		count++
	}
	
	return count, nil
}

// WritePacket implements stack.LinkEndpoint.WritePacket (single packet version).
func (l *LinkNIC) WritePacket(pkt *stack.PacketBuffer) tcpip.Error {
	count, err := l.WritePackets(stack.PacketBufferList{})
	pkt.DecRef()
	if count == 0 && err != nil {
		return err
	}
	return nil
}

// Wait implements stack.LinkEndpoint.Wait.
func (l *LinkNIC) Wait() {}

// ARPHardwareType implements stack.LinkEndpoint.ARPHardwareType.
func (l *LinkNIC) ARPHardwareType() header.ARPHardwareType {
	return header.ARPHardwareNone // No ARP for mesh P2P
}

// AddHeader implements stack.LinkEndpoint.AddHeader.
func (l *LinkNIC) AddHeader(pkt *stack.PacketBuffer) {}

// BuildAddress implements stack.LinkEndpoint.BuildAddress.
func (l *LinkNIC) BuildAddress(addr tcpip.Address) tcpip.LinkAddress {
	return "" // No link-layer address resolution
}
