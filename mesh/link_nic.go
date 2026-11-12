package mesh

import (
	"net"
	"sync"
	"sync/atomic"

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
	sender     PeerSender // Direct reference to the peer's sender (design §5.4)
	peerNodeID string     // Target node ID (for logging only)
	peerSubnet *net.IPNet // Peer's subnet for routing
	mtu        uint32

	// dispMu guards dispatcher: RemoveNIC calls Attach(nil) concurrently
	// with inbound delivery.
	dispMu     sync.RWMutex
	dispatcher stack.NetworkDispatcher

	// Statistics (atomic counters)
	injectPackets uint64 // Packets successfully injected into gVisor
	injectBytes   uint64 // Bytes successfully injected
	injectDrops   uint64 // Packets dropped (no dispatcher or invalid proto)
	sendFailures  uint64 // Egress packets the peer sender rejected
}

// NewLinkNIC creates a new LinkNIC for a direct mesh peer.
// The sender is the PeerSender that this LinkNIC will use to send packets (design §5.4).
func NewLinkNIC(nicID tcpip.NICID, sender PeerSender, peerNodeID string, peerSubnet *net.IPNet) *LinkNIC {
	return &LinkNIC{
		nicID:      nicID,
		sender:     sender,
		peerNodeID: peerNodeID,
		peerSubnet: peerSubnet,
		mtu:        1600, // IPIP outer header (20B) + inner up to 1400 (tun.OverlayMTU) + headroom; kept at 1600 so the stack has IPIP encap room, while the 1360 MSS clamp bounds inner packets
	}
}

// Attach implements stack.LinkEndpoint.Attach.
func (l *LinkNIC) Attach(dispatcher stack.NetworkDispatcher) {
	l.dispMu.Lock()
	defer l.dispMu.Unlock()
	l.dispatcher = dispatcher
}

// IsAttached implements stack.LinkEndpoint.IsAttached.
func (l *LinkNIC) IsAttached() bool {
	l.dispMu.RLock()
	defer l.dispMu.RUnlock()
	return l.dispatcher != nil
}

// InjectInbound delivers a raw IP packet received from the peer into the
// netstack via this NIC. The stack records pkt.NICID so conntrack can use it
// as OriginalInputNIC (fork patch #5) for DNAT reply routing (patch #6).
func (l *LinkNIC) InjectInbound(data []byte) {
	l.injectInbound(data, buffer.MakeWithData(data))
}

// InjectInboundOwned delivers data without copying. release runs exactly once:
// immediately if the frame is dropped here, otherwise when gVisor drops the
// last reference to the packet (D6 zero-copy).
func (l *LinkNIC) InjectInboundOwned(data []byte, release func()) {
	l.injectInbound(data, buffer.MakeWithExternalData(data, release))
}

func (l *LinkNIC) injectInbound(data []byte, payload buffer.Buffer) {
	// Snapshot and deliver outside the lock (as channel.Endpoint does) so
	// delivery never blocks RemoveNIC, which holds the stack lock.
	l.dispMu.RLock()
	d := l.dispatcher
	l.dispMu.RUnlock()
	if d == nil || len(data) == 0 {
		atomic.AddUint64(&l.injectDrops, 1)
		payload.Release()
		return
	}
	var proto tcpip.NetworkProtocolNumber
	switch data[0] >> 4 {
	case 4:
		proto = ipv4.ProtocolNumber
	case 6:
		proto = ipv6.ProtocolNumber
	default:
		atomic.AddUint64(&l.injectDrops, 1)
		payload.Release()
		return
	}

	// Module-level logging for LinkNIC inbound
	if len(data) >= 20 {
		srcIP := net.IP(data[12:16]).To4()
		dstIP := net.IP(data[16:20]).To4()
		protoNum := data[9]
		util.LogModuleDebug("linknic", "[LINKNIC-INBOUND] linkNIC=%d peer=%s len=%d proto=%d src=%s dst=%s",
			l.nicID, l.peerNodeID, len(data), protoNum, srcIP, dstIP)
	}

	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: payload,
	})
	d.DeliverNetworkPacket(proto, pkt)
	pkt.DecRef()
	atomic.AddUint64(&l.injectPackets, 1)
	atomic.AddUint64(&l.injectBytes, uint64(len(data)))
}

// MTU implements stack.LinkEndpoint.MTU.
func (l *LinkNIC) MTU() uint32 {
	return l.mtu
}

// NICID returns the LinkNIC's NIC ID.
func (l *LinkNIC) NICID() tcpip.NICID {
	return l.nicID
}

// PeerSubnet returns the peer's mesh subnet.
func (l *LinkNIC) PeerSubnet() *net.IPNet {
	return l.peerSubnet
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
// Sends packets directly via the held PeerSender (design §5.4).
// RouteSelector already chose this specific LinkNIC, so we send directly.
//
// The endpoint owns the packets (gVisor contract) and must DecRef each one.
// peer.Send queues asynchronously, so the payload is copied before the packet
// buffer is released.
func (l *LinkNIC) WritePackets(pkts stack.PacketBufferList) (int, tcpip.Error) {
	// One level check for all per-packet debug logs below — with debug off the
	// hot path pays nothing but the functional Flatten (audit item 9).
	debugOn := util.GetModuleLogLevel("linknic") <= util.LogLevelDebug
	if debugOn {
		util.LogModuleDebug("linknic", "[LINKNIC-WRITEPACKETS-ENTRY] linkNIC=%d peer=%s pktCount=%d selfPtr=%p", l.nicID, l.peerNodeID, pkts.Len(), l)
	}

	if l.sender == nil {
		return 0, &tcpip.ErrInvalidEndpointState{}
	}

	count := 0
	for i, pkt := range pkts.AsSlice() {
		buf := pkt.ToBuffer()
		pktData := buf.Flatten()
		if debugOn {
			util.LogModuleDebug("linknic", "[LINKNIC-WRITEPACKETS-LOOP] linkNIC=%d peer=%s pktIndex=%d", l.nicID, l.peerNodeID, i)
			util.LogModuleDebug("linknic", "[LINKNIC-WRITEPACKETS-DATA] linkNIC=%d peer=%s dataLen=%d", l.nicID, l.peerNodeID, len(pktData))

			// Log basic packet info for all packets (including IPIP)
			if len(pktData) >= 20 {
				version := pktData[0] >> 4
				proto := pktData[9]
				srcIP := net.IP(pktData[12:16]).To4()
				dstIP := net.IP(pktData[16:20]).To4()
				util.LogModuleDebug("linknic", "[LINKNIC-WRITEPACKETS] linkNIC=%d peer=%s len=%d version=%d proto=%d src=%s dst=%s",
					l.nicID, l.peerNodeID, len(pktData), version, proto, srcIP, dstIP)
			}
		}

		TraceForwarding("linknic_egress_start", pktData, "linkNIC=%d peer=%s", l.nicID, l.peerNodeID)
		// Send is queued asynchronously; copy, then release the ToBuffer
		// reference. The packet itself stays owned by the caller (same
		// contract as channel.Endpoint): the transport layer DecRefs it after
		// WritePacket returns, so the endpoint must not.
		payload := append([]byte(nil), pktData...)
		buf.Release()
		if err := l.sender.Send(payload); err != nil {
			TraceForwarding("linknic_egress_error", payload, "linkNIC=%d peer=%s err=%v", l.nicID, l.peerNodeID, err)
			util.LogModuleWarn("linknic", "[LINKNIC] send via peer %s failed: %v", l.peerNodeID, err)
			atomic.AddUint64(&l.sendFailures, 1)
			continue
		}
		TraceForwarding("linknic_egress", payload, "linkNIC=%d peer=%s", l.nicID, l.peerNodeID)
		count++
	}

	return count, nil
}

// WritePacket implements stack.LinkEndpoint.WritePacket (single packet version).
func (l *LinkNIC) WritePacket(r *stack.Route, pkt *stack.PacketBuffer) tcpip.Error {
	var list stack.PacketBufferList
	list.PushBack(pkt)

	// No pre-Flatten for logging here — WritePackets flattens once per packet
	// for the actual send, and its guarded debug logs cover per-packet info
	// (audit item 8: this path previously flattened twice per packet).
	n, err := l.WritePackets(list)
	if n == 0 {
		if err != nil {
			return err
		}
		return &tcpip.ErrAborted{}
	}
	return nil
}

// InjectStats returns the injection statistics for this LinkNIC.
func (l *LinkNIC) InjectStats() (packets, bytes, drops uint64) {
	return atomic.LoadUint64(&l.injectPackets),
		atomic.LoadUint64(&l.injectBytes),
		atomic.LoadUint64(&l.injectDrops)
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
