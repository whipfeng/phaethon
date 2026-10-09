package mesh

import (
	"net"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"phaethon/util"
)

// LinkNIC is a "dumb" NIC for direct mesh peer communication.
// It only handles sending packets to the associated peer, and injecting
// received peer frames into the netstack (design §5.2).
// No IPIP encapsulation, no routing decisions - the netstack does those.
type LinkNIC struct {
	nicID      tcpip.NICID
	peerNodeID string
	peerSubnet *net.IPNet // Peer's subnet for routing
	mtu        uint32
	meshMgr    *MeshManager
	dispatcher stack.NetworkDispatcher
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
	l.dispatcher = dispatcher
}

// IsAttached implements stack.LinkEndpoint.IsAttached.
func (l *LinkNIC) IsAttached() bool {
	return l.dispatcher != nil
}

// InjectInbound delivers a raw IP packet received from the peer into the
// netstack via this NIC. The stack records pkt.NICID so conntrack can use it
// as OriginalInputNIC (fork patch #5) for DNAT reply routing (patch #6).
func (l *LinkNIC) InjectInbound(data []byte) {
	if l.dispatcher == nil || len(data) == 0 {
		return
	}
	var proto tcpip.NetworkProtocolNumber
	switch data[0] >> 4 {
	case 4:
		proto = ipv4.ProtocolNumber
	case 6:
		proto = ipv6.ProtocolNumber
	default:
		return
	}
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(data),
	})
	l.dispatcher.DeliverNetworkPacket(proto, pkt)
	pkt.DecRef()
}

// MTU implements stack.LinkEndpoint.MTU.
func (l *LinkNIC) MTU() uint32 {
	return l.mtu
}

// SetMTU implements stack.NetworkLinkEndpoint.SetMTU.
func (l *LinkNIC) SetMTU(mtu uint32) {
	l.mtu = mtu
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

// SetLinkAddress implements stack.NetworkLinkEndpoint.SetLinkAddress.
func (l *LinkNIC) SetLinkAddress(addr tcpip.LinkAddress) {}

// WritePackets implements stack.LinkEndpoint.WritePackets.
// Sends packets toward the node that owns each packet's destination IP; the
// mesh hop table picks the next hop at send time (direct or relayed, design
// §5.2). Routes for multi-hop nodes land on the next-hop peer's Link NIC, so
// the bound peer is only a fallback.
//
// Packet buffers remain owned by the caller (same contract as channel.Endpoint,
// which clones before queueing); we only read the data synchronously.
func (l *LinkNIC) WritePackets(pkts stack.PacketBufferList) (int, tcpip.Error) {
	if l.meshMgr == nil {
		return 0, &tcpip.ErrInvalidEndpointState{}
	}

	count := 0
	for _, pkt := range pkts.AsSlice() {
		buf := pkt.ToBuffer()
		pktData := buf.Flatten()

		// Resolve the FINAL target node from the packet's destination
		// (inner dst for plain frames, outer dst for IPIP frames — both are
		// node-subnet addresses). SendToNode then routes via the hop table.
		target := l.peerNodeID
		if len(pktData) >= 20 && pktData[0]>>4 == 4 {
			if nodeID := l.meshMgr.ResolveNodeIDForIP(net.IP(pktData[16:20])); nodeID != "" {
				target = nodeID
			}
		}

		TraceForwarding("linknic_egress_start", pktData, "linkNIC=%d boundPeer=%s target=%s", l.nicID, l.peerNodeID, target)
		if err := l.meshMgr.SendToNode(target, pktData); err != nil {
			TraceForwarding("linknic_egress_error", pktData, "linkNIC=%d boundPeer=%s target=%s err=%v", l.nicID, l.peerNodeID, target, err)
			util.LogDebug("[LINKNIC] send to node %s failed: %v", target, err)
			continue
		}
		TraceForwarding("linknic_egress", pktData, "linkNIC=%d boundPeer=%s target=%s", l.nicID, l.peerNodeID, target)
		count++
	}

	return count, nil
}

// WritePacket implements stack.LinkEndpoint.WritePacket (single packet version).
func (l *LinkNIC) WritePacket(pkt *stack.PacketBuffer) tcpip.Error {
	var list stack.PacketBufferList
	list.PushBack(pkt)
	n, err := l.WritePackets(list)
	if n == 0 {
		if err != nil {
			return err
		}
		return &tcpip.ErrAborted{}
	}
	return nil
}

// Wait implements stack.LinkEndpoint.Wait.
func (l *LinkNIC) Wait() {}

// ParseHeader implements stack.NetworkLinkEndpoint.ParseHeader.
func (l *LinkNIC) ParseHeader(*stack.PacketBuffer) bool {
	return true // No link-layer header to parse
}

// Close implements stack.LinkEndpoint.Close.
func (l *LinkNIC) Close() {}

// SetOnCloseAction implements stack.LinkEndpoint.SetOnCloseAction.
func (l *LinkNIC) SetOnCloseAction(f func()) {}

// ARPHardwareType implements stack.LinkEndpoint.ARPHardwareType.
func (l *LinkNIC) ARPHardwareType() header.ARPHardwareType {
	return header.ARPHardwareNone // No ARP for mesh P2P
}

// AddHeader implements stack.LinkEndpoint.AddHeader.
func (l *LinkNIC) AddHeader(pkt *stack.PacketBuffer) {}
