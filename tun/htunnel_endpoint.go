package tun

import (
	"sync"

	"phaethon/frame"
	"phaethon/util"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

var _ stack.LinkEndpoint = (*hTunnelEndpoint)(nil)

// hTunnelEndpoint is a gVisor link.Endpoint for h_tunnel uplink only.
// It wraps outbound IP packets as FrameMeshPacket frames and sends them
// through the existing mesh channel (FrameTransport).
//
// Downlink is handled by the existing P2P session:
// transport.Recv → HandleMeshFrame → InjectMeshPacket → netstack
//
// No recvLoop needed here — the P2P session already processes incoming
// frames and injects them into the netstack via the main mesh endpoint.
type hTunnelEndpoint struct {
	mu        sync.RWMutex
	mtu       uint32
	nicID     tcpip.NICID
	transport frame.FrameTransport
	closed    bool
}

// newHTunnelEndpoint creates an endpoint that sends IP packets as
// FrameMeshPacket frames through the given FrameTransport.
func newHTunnelEndpoint(nicID tcpip.NICID, transport frame.FrameTransport) *hTunnelEndpoint {
	return &hTunnelEndpoint{
		nicID:     nicID,
		mtu:       1500,
		transport: transport,
	}
}

// WritePackets sends outbound IP packets as FrameMeshPacket frames via the transport.
func (e *hTunnelEndpoint) WritePackets(pkts stack.PacketBufferList) (int, tcpip.Error) {
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

		if err := e.transport.Send(frame.FrameMeshPacket, data); err != nil {
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
func (e *hTunnelEndpoint) Attach(stack.NetworkDispatcher) {}

// IsAttached always returns true (uplink-only endpoint).
func (e *hTunnelEndpoint) IsAttached() bool { return true }

func (e *hTunnelEndpoint) MTU() uint32                         { return e.mtu }
func (e *hTunnelEndpoint) SetMTU(mtu uint32)                   { e.mtu = mtu }
func (e *hTunnelEndpoint) Capabilities() stack.LinkEndpointCapabilities { return 0 }
func (e *hTunnelEndpoint) MaxHeaderLength() uint16             { return 0 }
func (e *hTunnelEndpoint) LinkAddress() tcpip.LinkAddress      { return "" }
func (e *hTunnelEndpoint) SetLinkAddress(tcpip.LinkAddress)    {}
func (e *hTunnelEndpoint) ARPHardwareType() header.ARPHardwareType {
	return header.ARPHardwareNone
}
func (e *hTunnelEndpoint) AddHeader(*stack.PacketBuffer)          {}
func (e *hTunnelEndpoint) ParseHeader(*stack.PacketBuffer) bool   { return true }
func (e *hTunnelEndpoint) Wait()                                  {}
func (e *hTunnelEndpoint) SetOnCloseAction(func())                {}

// Close marks the endpoint as closed and closes the transport.
func (e *hTunnelEndpoint) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.closed {
		e.closed = true
		e.transport.Close()
	}
}
