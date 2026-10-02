package mesh

import (
	"fmt"
	"net"
	"sync"

	"phaethon/config"
	"phaethon/util"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// LoopbackEndpoint implements a loopback link endpoint for NIC 3.
// It is the unified dispatch point per multi_nic_architecture_v2 design.
// 
// Dispatch logic (in order):
// 1. src ∉ mesh network → send to TUN device
// 2. src ∈ mesh network:
//    a. IPIP packet (proto=4, outer dst=local GIP) → decapsulate → loopback to Forwarder
//    b. Non-IPIP packet:
//       - dst hits announced routes → IPIP encapsulate → send via mesh
//       - dst doesn't hit routes → loopback to Forwarder
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

	// IPIP encapsulation support
	ipipTunnel     *IPIPTunnel
	meshManager    *MeshManager
	staticRoutes   []config.MeshStaticRoute
	localEIP       tcpip.Address
	
	// Mesh subnet for source IP classification
	meshSubnet *net.IPNet
	
	// Local GIP for IPIP decapsulation check
	localGIP net.IP
	
	// TUN device for sending non-mesh packets
	tunDevice interface {
		Write(data []byte) (int, error)
	}
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

// SetIPIPConfig configures IPIP encapsulation parameters
func (e *LoopbackEndpoint) SetIPIPConfig(tunnel *IPIPTunnel, meshMgr *MeshManager, routes []config.MeshStaticRoute, localEIP tcpip.Address) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ipipTunnel = tunnel
	e.meshManager = meshMgr
	e.staticRoutes = routes
	e.localEIP = localEIP
}

// SetDispatchConfig configures the unified dispatch parameters
func (e *LoopbackEndpoint) SetDispatchConfig(meshSubnet *net.IPNet, localGIP net.IP, tunDevice interface{ Write([]byte) (int, error) }) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.meshSubnet = meshSubnet
	e.localGIP = localGIP
	e.tunDevice = tunDevice
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

// SetMTU sets the maximum transmission unit
func (e *LoopbackEndpoint) SetMTU(mtu uint32) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.mtu = mtu
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

// SetLinkAddress sets the link layer address
func (e *LoopbackEndpoint) SetLinkAddress(addr tcpip.LinkAddress) {
	// Loopback doesn't use link addresses
}

// WritePackets is called by the netstack to send packets.
// Implements the unified dispatch logic per multi_nic_architecture_v2 design.
func (e *LoopbackEndpoint) WritePackets(pkts stack.PacketBufferList) (int, tcpip.Error) {
	e.mu.RLock()
	dispatcher := e.dispatcher
	ipipTunnel := e.ipipTunnel
	meshMgr := e.meshManager
	staticRoutes := e.staticRoutes
	localEIP := e.localEIP
	meshSubnet := e.meshSubnet
	localGIP := e.localGIP
	tunDevice := e.tunDevice
	e.mu.RUnlock()

	if dispatcher == nil {
		return 0, &tcpip.ErrInvalidEndpointState{}
	}

	count := 0
	for _, pkt := range pkts.AsSlice() {
		e.stats.PacketsReceived++
		e.stats.BytesReceived += uint64(pkt.Size())

		// Extract packet info
		buf := pkt.ToBuffer()
		data := buf.Flatten()
		
		if len(data) < 20 {
			util.LogWarn("[LOOPBACK] packet too short: %d bytes", len(data))
			continue
		}
		
		srcIP := net.IP(data[12:16])
		dstIP := net.IP(data[16:20])
		proto := data[9]
		
		if e.stats.PacketsReceived <= 10 {
			util.LogDebug("[LOOPBACK] packet #%d: src=%v dst=%v proto=%d",
				e.stats.PacketsReceived, srcIP, dstIP, proto)
		}
		
		// Step 1: Check if src ∈ mesh network
		if meshSubnet != nil && !meshSubnet.Contains(srcIP) {
			// src ∉ mesh → send to TUN device
			if tunDevice != nil {
				if _, err := tunDevice.Write(data); err != nil {
					util.LogWarn("[LOOPBACK] failed to write to TUN: %v", err)
				} else {
					e.stats.PacketsLooped++
					e.stats.BytesLooped += uint64(len(data))
					count++
				}
			}
			continue
		}
		
		// Step 2: src ∈ mesh network
		// Check if it's an IPIP packet (proto=4)
		if proto == 4 && localGIP != nil && dstIP.Equal(localGIP) {
			// IPIP packet destined for local GIP → decapsulate
			innerPacket, err := Decapsulate(data)
			if err == nil && innerPacket != nil {
				// Loopback decapsulated packet to Forwarder
				newPkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
					Payload: buffer.MakeWithData(innerPacket),
				})
				dispatcher.DeliverNetworkPacket(pkt.NetworkProtocolNumber, newPkt)
				newPkt.DecRef()
				
				e.stats.PacketsLooped++
				e.stats.BytesLooped += uint64(len(innerPacket))
				count++
				continue
			}
			// If decapsulation failed, drop the packet
			util.LogWarn("[LOOPBACK] IPIP decapsulation failed for packet from %v: %v", srcIP, err)
			continue
		}
		
		// Step 3: Non-IPIP packet from mesh
		// Check if dst hits announced routes (static routes)
		if ipipTunnel != nil && meshMgr != nil && e.needsIPIPEncapsulation(dstIP, staticRoutes) {
			// dst hits announced routes → IPIP encapsulate → send via mesh
			encapsulated, err := e.encapsulatePacket(pkt, ipipTunnel, meshMgr, localEIP, dstIP)
			if err == nil && encapsulated != nil {
				e.sendViaMesh(encapsulated, meshMgr, dstIP)
				e.stats.PacketsEncapsulated++
				count++
				continue
			}
			// If encapsulation failed, fall through to loopback
			util.LogWarn("[LOOPBACK] IPIP encapsulation failed for dst=%v, falling back to loopback", dstIP)
		}
		
		// Step 4: dst doesn't hit announced routes → loopback to Forwarder
		newPkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
			Payload: buffer.MakeWithData(data),
		})
		dispatcher.DeliverNetworkPacket(pkt.NetworkProtocolNumber, newPkt)
		newPkt.DecRef()
		
		e.stats.PacketsLooped++
		e.stats.BytesLooped += uint64(len(data))
		count++
	}

	return count, nil
}

// needsIPIPEncapsulation checks if a packet needs IPIP encapsulation
func (e *LoopbackEndpoint) needsIPIPEncapsulation(dstIP net.IP, routes []config.MeshStaticRoute) bool {
	for _, route := range routes {
		_, network, err := net.ParseCIDR(route.Prefix)
		if err != nil {
			continue
		}
		if network.Contains(dstIP) {
			return true
		}
	}
	return false
}

// encapsulatePacket performs IPIP encapsulation on a packet
func (e *LoopbackEndpoint) encapsulatePacket(pkt *stack.PacketBuffer, tunnel *IPIPTunnel, meshMgr *MeshManager, localEIP tcpip.Address, dstIP net.IP) ([]byte, error) {
	// Get the raw packet data using ToBuffer
	buf := pkt.ToBuffer()
	rawData := buf.Flatten()
	
	return e.encapsulateRawData(rawData, tunnel, meshMgr, localEIP, dstIP)
}

// encapsulateRawData performs IPIP encapsulation on raw packet data
func (e *LoopbackEndpoint) encapsulateRawData(rawData []byte, tunnel *IPIPTunnel, meshMgr *MeshManager, localEIP tcpip.Address, dstIP net.IP) ([]byte, error) {
	if len(rawData) < 20 {
		return nil, fmt.Errorf("packet too short: %d bytes", len(rawData))
	}
	
	// Select egress node and get its EIP
	egressNodeID, egressEIP, err := meshMgr.SelectEgressNodeIDForIP(dstIP)
	if err != nil {
		return nil, fmt.Errorf("no egress node for %v: %w", dstIP, err)
	}
	if egressNodeID == "" {
		return nil, fmt.Errorf("no egress node for %v", dstIP)
	}
	if len(egressEIP) == 0 {
		return nil, fmt.Errorf("no EIP for egress node %s", egressNodeID)
	}
	
	// Perform IPIP encapsulation
	encapsulated, err := tunnel.Encapsulate(localEIP.AsSlice(), egressEIP, rawData)
	if err != nil {
		return nil, err
	}
	
	return encapsulated, nil
}

// sendViaMesh sends an encapsulated packet via the mesh network
func (e *LoopbackEndpoint) sendViaMesh(data []byte, meshMgr *MeshManager, dstIP net.IP) {
	// Send the encapsulated packet via mesh network
	// The mesh manager will handle routing it to the correct peer
	if err := meshMgr.SendEncapsulatedPacket(data, dstIP); err != nil {
		util.LogWarn("[LOOPBACK] Failed to send encapsulated packet: %v", err)
	}
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

// SetOnCloseAction sets the action that will be executed before closing the endpoint
func (e *LoopbackEndpoint) SetOnCloseAction(func()) {
	// Loopback doesn't need a close action
}

// Stats returns the loopback endpoint statistics
func (e *LoopbackEndpoint) Stats() LoopbackStats {
	return e.stats
}
