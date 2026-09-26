package mesh

import (
	"encoding/binary"
	"fmt"
	"net"
	"phaethon/config"
	"phaethon/util"
	"strings"
	"sync"
)

// IPIPTunnel handles IP-in-IP encapsulation for policy routing
type IPIPTunnel struct {
	mu        sync.RWMutex
	localEIP  net.IP           // Local node's EIP (Egress IP)
	nodeEIPs  map[string]net.IP // nodeID -> EIP mapping
	enabled   bool
}

// NewIPIPTunnel creates a new IPIP tunnel handler
func NewIPIPTunnel() *IPIPTunnel {
	return &IPIPTunnel{
		nodeEIPs: make(map[string]net.IP),
		enabled:  false,
	}
}

// SetLocalEIP sets the local node's EIP
func (t *IPIPTunnel) SetLocalEIP(eip net.IP) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.localEIP = eip
	t.enabled = eip != nil
	util.LogInfo("[IPIP] Local EIP set to %s", eip)
}

// GetLocalEIP returns the local node's EIP
func (t *IPIPTunnel) GetLocalEIP() net.IP {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.localEIP
}

// SetNodeEIP sets the EIP for a specific node
func (t *IPIPTunnel) SetNodeEIP(nodeID string, eip net.IP) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.nodeEIPs[nodeID] = eip
	util.LogInfo("[IPIP] Node %s EIP set to %s", nodeID, eip)
}

// GetNodeEIP returns the EIP for a specific node
func (t *IPIPTunnel) GetNodeEIP(nodeID string) net.IP {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.nodeEIPs[nodeID]
}

// CalculateEIP calculates the EIP for a given subnet
// EIP is subnet + 4 (e.g., 100.0.0.4 for 100.0.0.0/16)
// Reserved IPs: .0=network, .1=VIP, .2=hostIP, .3=GIP, .4=EIP, .5-.10=future
// This avoids conflicts with Fake-IP allocation which starts after the reserved range.
func CalculateEIP(subnet *net.IPNet) net.IP {
	if subnet == nil {
		return nil
	}
	
	ip := subnet.IP.To4()
	if ip == nil {
		return nil // IPv6 not supported yet
	}
	
	// EIP is subnet + 4
	eip := make(net.IP, 4)
	copy(eip, ip)
	eip[3] = ip[3] + 4
	
	return eip
}

// AllocateEIP allocates an EIP from the given subnet
// EIP is the last usable IP in the subnet (e.g., 100.0.0.254 for 100.0.0.0/24)
// Deprecated: Use CalculateEIP instead
func AllocateEIP(subnet *net.IPNet) net.IP {
	return CalculateEIP(subnet)
}

// Encapsulate wraps a packet with an IPIP header
// outerSrc: local node's EIP
// outerDst: egress node's EIP
// innerPacket: original IP packet
func (t *IPIPTunnel) Encapsulate(outerSrc, outerDst net.IP, innerPacket []byte) ([]byte, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	
	if !t.enabled {
		return nil, fmt.Errorf("IPIP tunnel not enabled")
	}
	
	// Create IPIP header (20 bytes, no options)
	outerHeader := make([]byte, 20)
	
	// Version (4) + IHL (5) = 0x45
	outerHeader[0] = 0x45
	
	// DSCP/ECN (0)
	outerHeader[1] = 0
	
	// Total length = outer header (20) + inner packet length
	totalLen := 20 + len(innerPacket)
	binary.BigEndian.PutUint16(outerHeader[2:4], uint16(totalLen))
	
	// Identification (0 for now)
	binary.BigEndian.PutUint16(outerHeader[4:6], 0)
	
	// Flags + Fragment offset (0)
	binary.BigEndian.PutUint16(outerHeader[6:8], 0)
	
	// TTL (64)
	outerHeader[8] = 64
	
	// Protocol (4 = IPIP)
	outerHeader[9] = 4
	
	// Header checksum (0 for now, will calculate)
	outerHeader[10] = 0
	outerHeader[11] = 0
	
	// Source IP
	copy(outerHeader[12:16], outerSrc.To4())
	
	// Destination IP
	copy(outerHeader[16:20], outerDst.To4())
	
	// Calculate header checksum
	checksum := ipChecksum(outerHeader)
	binary.BigEndian.PutUint16(outerHeader[10:12], checksum)
	
	// Combine outer header + inner packet
	result := make([]byte, totalLen)
	copy(result[0:20], outerHeader)
	copy(result[20:], innerPacket)
	
	return result, nil
}

// Decapsulate strips the outer IPIP header and returns the inner packet
func Decapsulate(packet []byte) ([]byte, error) {
	if len(packet) < 20 {
		return nil, fmt.Errorf("packet too short for IPIP header")
	}
	
	// Check version (should be 4)
	version := packet[0] >> 4
	if version != 4 {
		return nil, fmt.Errorf("not an IPv4 packet")
	}
	
	// Check protocol (should be 4 for IPIP)
	protocol := packet[9]
	if protocol != 4 {
		return nil, fmt.Errorf("not an IPIP packet (protocol=%d)", protocol)
	}
	
	// Get IHL (Internet Header Length)
	ihl := int(packet[0]&0x0f) * 4
	if len(packet) < ihl {
		return nil, fmt.Errorf("packet too short for header length")
	}
	
	// Return inner packet (skip outer header)
	return packet[ihl:], nil
}

// ipChecksum calculates the IP header checksum
func ipChecksum(header []byte) uint16 {
	var sum uint32
	length := len(header)
	
	for i := 0; i < length-1; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(header[i:]))
	}
	
	if length%2 == 1 {
		sum += uint32(header[length-1]) << 8
	}
	
	for sum > 0xffff {
		sum = (sum >> 16) + (sum & 0xffff)
	}
	
	return ^uint16(sum)
}

// MatchStaticRoute checks if a destination IP matches any static IPIP route
// Returns (nodeIDs, true) if matched, (nil, false) otherwise
func (t *IPIPTunnel) MatchStaticRoute(dstIP net.IP, staticRoutes []config.MeshStaticRoute) ([]string, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	
	for _, route := range staticRoutes {
		_, network, err := net.ParseCIDR(route.Prefix)
		if err != nil {
			continue
		}
		if network.Contains(dstIP) {
			return route.NodeIDs, true
		}
	}
	return nil, false
}

// MatchStaticDomainSuffix checks if a domain matches any static domain suffix route
// Returns (nodeIDs, true) if matched, (nil, false) otherwise
func (t *IPIPTunnel) MatchStaticDomainSuffix(domain string, staticSuffixes []config.MeshStaticDomainSuffix) ([]string, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	
	domain = strings.ToLower(domain)
	for _, suffix := range staticSuffixes {
		s := strings.ToLower(suffix.Suffix)
		// Match if domain equals suffix or ends with "." + suffix
		if domain == s || strings.HasSuffix(domain, "."+s) {
			return suffix.NodeIDs, true
		}
	}
	return nil, false
}
