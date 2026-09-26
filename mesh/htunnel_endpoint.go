package mesh

import (
	"sync"

	"phaethon/util"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

var _ stack.LinkEndpoint = (*HTunnelEndpoint)(nil)

// HTunnelEndpoint is a gVisor link.Endpoint for h_tunnel uplink only.
// It sends outbound IP packets through the peer's send queue (peer.writeCh),
// unifying the sending path with regular mesh traffic.
//
// Downlink is handled by the existing P2P session:
// transport.Recv → HandleMeshFrame → InjectMeshPacket → netstack
//
// No recvLoop needed here — the P2P session already processes incoming
// frames and injects them into the netstack via the main mesh endpoint.
type HTunnelEndpoint struct {
	mu     sync.RWMutex
	mtu    uint32
	nicID  tcpip.NICID
	peer   PeerSender // peer to send packets through
	closed bool
}

// NewHTunnelEndpoint creates an endpoint that sends IP packets through the
// given peer's send queue (peer.writeCh → peerWriteLoop → transport.Send).
func NewHTunnelEndpoint(nicID tcpip.NICID, peer PeerSender) *HTunnelEndpoint {
	return &HTunnelEndpoint{
		nicID: nicID,
		mtu:   1500,
		peer:  peer,
	}
}

// WritePackets sends outbound IP packets through the peer's send queue.
func (e *HTunnelEndpoint) WritePackets(pkts stack.PacketBufferList) (int, tcpip.Error) {
	e.mu.RLock()
	if e.closed {
		e.mu.RUnlock()
		return 0, &tcpip.ErrClosedForSend{}
	}
	e.mu.RUnlock()

	n := 0
	for _, pkt := range pkts.AsSlice() {
		buf := pkt.ToBuffer()
		data := buf.Flatten()

		if len(data) < 20 {
			pkt.DecRef()
			continue
		}

		if err := e.peer.Send(data); err != nil {
			util.LogWarn("[HTUNNEL-EP] send fail: %v", err)
			pkt.DecRef()
			if n == 0 {
				return 0, &tcpip.ErrAborted{}
			}
			break
		}
		n++
		pkt.DecRef()
	}
	return n, nil
}

// Attach is a no-op — downlink is handled by the existing P2P session.
func (e *HTunnelEndpoint) Attach(stack.NetworkDispatcher) {}

// IsAttached always returns true (uplink-only endpoint).
func (e *HTunnelEndpoint) IsAttached() bool { return true }

func (e *HTunnelEndpoint) MTU() uint32                               { return e.mtu }
func (e *HTunnelEndpoint) SetMTU(mtu uint32)                         { e.mtu = mtu }
func (e *HTunnelEndpoint) Capabilities() stack.LinkEndpointCapabilities { return 0 }
func (e *HTunnelEndpoint) MaxHeaderLength() uint16                   { return 0 }
func (e *HTunnelEndpoint) LinkAddress() tcpip.LinkAddress            { return "" }
func (e *HTunnelEndpoint) SetLinkAddress(tcpip.LinkAddress)          {}
func (e *HTunnelEndpoint) ARPHardwareType() header.ARPHardwareType {
	return header.ARPHardwareNone
}
func (e *HTunnelEndpoint) AddHeader(*stack.PacketBuffer)        {}
func (e *HTunnelEndpoint) ParseHeader(*stack.PacketBuffer) bool { return true }
func (e *HTunnelEndpoint) Wait()                                {}
func (e *HTunnelEndpoint) SetOnCloseAction(func())              {}

// NicID returns the NIC ID assigned to this endpoint.
func (e *HTunnelEndpoint) NicID() tcpip.NICID { return e.nicID }

// Close marks the endpoint as closed.
// Note: peer lifecycle is managed by the P2P layer, not here.
func (e *HTunnelEndpoint) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
}
