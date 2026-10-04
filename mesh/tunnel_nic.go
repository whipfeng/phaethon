package mesh

import (
	"fmt"
	"phaethon/util"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// TunnelNIC implements stack.LinkEndpoint for IPIP tunnel encapsulation.
// Each TunnelNIC is associated with a target egress node and encapsulates
// packets with IPIP header before sending via mesh.
type TunnelNIC struct {
	// Immutable fields
	nicID       tcpip.NICID
	targetNodeID string
	targetEIP   tcpip.Address // Target node's EIP (outer destination)
	localEIP    tcpip.Address // Local EIP (outer source)
	mtu         uint32

	// Mesh manager for sending encapsulated packets
	mesh *MeshManager

	// Callback for packet arrival (not used for tunnel NICs, but required by interface)
	dispatcher stack.NetworkDispatcher
}

// NewTunnelNIC creates a new tunnel NIC for IPIP encapsulation.
func NewTunnelNIC(nicID tcpip.NICID, targetNodeID string, targetEIP, localEIP tcpip.Address, mtu uint32, mesh *MeshManager) *TunnelNIC {
	return &TunnelNIC{
		nicID:        nicID,
		targetNodeID: targetNodeID,
		targetEIP:    targetEIP,
		localEIP:     localEIP,
		mtu:          mtu,
		mesh:         mesh,
	}
}

// Attach saves the stack.NetworkDispatcher for link-layer packets.
func (t *TunnelNIC) Attach(dispatcher stack.NetworkDispatcher) {
	t.dispatcher = dispatcher
}

// IsAttached returns whether the NIC is attached.
func (t *TunnelNIC) IsAttached() bool {
	return t.dispatcher != nil
}

// NICID returns the NIC ID.
func (t *TunnelNIC) NICID() tcpip.NICID {
	return t.nicID
}

// Close closes the tunnel NIC (no-op for tunnel NICs).
func (t *TunnelNIC) Close() {
	// No resources to release
}

// Wait waits for worker goroutines to stop (no-op for tunnel NICs).
func (t *TunnelNIC) Wait() {
	// No worker goroutines
}

// ARPHardwareType returns the ARP hardware type (ARPHRD_NONE for tunnel).
func (t *TunnelNIC) ARPHardwareType() header.ARPHardwareType {
	return header.ARPHardwareNone
}

// SetOnCloseAction sets the close action (no-op for tunnel NICs).
func (t *TunnelNIC) SetOnCloseAction(func()) {
	// No-op for tunnel NICs
}

// MTU returns the MTU of the tunnel.
func (t *TunnelNIC) MTU() uint32 {
	// Inner MTU = outer MTU - outer IP header (20 bytes)
	return t.mtu - 20
}

// SetMTU sets the MTU (no-op for tunnel NICs, MTU is fixed).
func (t *TunnelNIC) SetMTU(mtu uint32) {
	// No-op for tunnel NICs
}

// LinkAddress returns the link-layer address (not used for tunnel).
func (t *TunnelNIC) LinkAddress() tcpip.LinkAddress {
	return ""
}

// SetLinkAddress sets the link-layer address (no-op for tunnel NICs).
func (t *TunnelNIC) SetLinkAddress(addr tcpip.LinkAddress) {
	// No-op for tunnel NICs
}

// Capabilities returns the capabilities of the tunnel NIC.
func (t *TunnelNIC) Capabilities() stack.LinkEndpointCapabilities {
	return stack.CapabilityNone
}

// MaxHeaderLength returns the maximum header length (outer IP header).
func (t *TunnelNIC) MaxHeaderLength() uint16 {
	return header.IPv4MinimumSize
}

// WritePackets encapsulates packets with IPIP header and sends via mesh.
func (t *TunnelNIC) WritePackets(pkts stack.PacketBufferList) (int, tcpip.Error) {
	count := 0
	for _, pkt := range pkts.AsSlice() {
		// Get the inner packet data
		buf := pkt.ToBuffer()
		innerData := buf.Flatten()
		
		// Encapsulate with IPIP header
		encapsulated, err := t.encapsulate(innerData)
		if err != nil {
			util.LogWarn("[TUNNEL-NIC] encapsulation failed for target %s: %v", t.targetNodeID, err)
			pkt.DecRef()
			continue
		}
		
		// Send via mesh to the target node
		if t.mesh != nil {
			if err := t.mesh.SendToNode(t.targetNodeID, encapsulated); err != nil {
				util.LogWarn("[TUNNEL-NIC] send to target %s failed: %v", t.targetNodeID, err)
				pkt.DecRef()
				continue
			}
			util.LogDebug("[TUNNEL-NIC] sent encapsulated packet to target %s: inner=%d outer=%d",
				t.targetNodeID, len(innerData), len(encapsulated))
		} else {
			util.LogWarn("[TUNNEL-NIC] mesh manager is nil")
			pkt.DecRef()
			continue
		}
		
		pkt.DecRef()
		count++
	}
	return count, nil
}

// encapsulate wraps a packet with an IPIP header.
func (t *TunnelNIC) encapsulate(innerPacket []byte) ([]byte, error) {
	// Create IPIP header (20 bytes, no options)
	outerHeader := make([]byte, header.IPv4MinimumSize)
	
	// Version (4) + IHL (5) = 0x45
	outerHeader[0] = 0x45
	
	// DSCP/ECN (0)
	outerHeader[1] = 0
	
	// Total length = outer header (20) + inner packet length
	totalLen := header.IPv4MinimumSize + len(innerPacket)
	if totalLen > 65535 {
		return nil, fmt.Errorf("packet too large: %d", totalLen)
	}
	outerHeader[2] = byte(totalLen >> 8)
	outerHeader[3] = byte(totalLen)
	
	// Identification (0 for now)
	outerHeader[4] = 0
	outerHeader[5] = 0
	
	// Flags + Fragment offset (0)
	outerHeader[6] = 0
	outerHeader[7] = 0
	
	// TTL (64)
	outerHeader[8] = 64
	
	// Protocol (4 = IPIP)
	outerHeader[9] = 4
	
	// Header checksum (0 for now, will calculate)
	outerHeader[10] = 0
	outerHeader[11] = 0
	
	// Source IP (local EIP)
	copy(outerHeader[12:16], t.localEIP.AsSlice())
	
	// Destination IP (target EIP)
	copy(outerHeader[16:20], t.targetEIP.AsSlice())
	
	// Calculate header checksum
	checksum := ipChecksum(outerHeader)
	outerHeader[10] = byte(checksum >> 8)
	outerHeader[11] = byte(checksum)
	
	// Combine outer header + inner packet
	result := make([]byte, totalLen)
	copy(result[0:header.IPv4MinimumSize], outerHeader)
	copy(result[header.IPv4MinimumSize:], innerPacket)
	
	return result, nil
}

// AddHeader adds a link-layer header (not used for tunnel).
func (t *TunnelNIC) AddHeader(pkt *stack.PacketBuffer) {}

// ParseHeader parses a link-layer header (not used for tunnel).
func (t *TunnelNIC) ParseHeader(pkt *stack.PacketBuffer) bool {
	return true
}
