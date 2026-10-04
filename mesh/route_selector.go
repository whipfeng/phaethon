package mesh

import (
	"net"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// RouteSelectorConfig holds configuration for the RouteSelector.
type RouteSelectorConfig struct {
	// Mesh subnet (e.g., 100.64.0.0/10)
	MeshSubnet *net.IPNet

	// Local node's VIP (subnet + 1)
	LocalVIP tcpip.Address

	// Local node's EIP (subnet + 4)
	LocalEIP tcpip.Address

	// IsFakeIP checks if an IP is a fakeIP (DNS-mapped virtual IP)
	IsFakeIP func(ip net.IP) bool

	// SelectEgressNode selects the egress node for a given destination
	// Returns (nodeVIP, found)
	SelectEgressNode func(dst net.IP) (tcpip.Address, bool)
}

// NewRouteSelector creates a RouteSelector function with the given configuration.
// The RouteSelector is called during FindRoute to make dynamic routing decisions.
func NewRouteSelector(cfg *RouteSelectorConfig) stack.RouteSelector {
	return func(dst tcpip.Address) stack.RouteDecision {
		dstIP := net.IP(dst.AsSlice())

		// 1. fakeIP → local delivery (to Forwarder for domain resolution)
		if cfg.IsFakeIP != nil && cfg.IsFakeIP(dstIP) {
			return stack.RouteDecision{
				LocalDelivery: true,
				Cacheable:     true,
			}
		}

		// 2. Mesh subnet → let route table handle it (direct peer routes)
		if cfg.MeshSubnet != nil && cfg.MeshSubnet.Contains(dstIP) {
			// Return empty decision - route table will match peer subnets
			return stack.RouteDecision{}
		}

		// 3. Non-mesh destination → IPIP encapsulation
		if cfg.SelectEgressNode != nil {
			egressVIP, found := cfg.SelectEgressNode(dstIP)
			if found {
				return stack.RouteDecision{
					NeedIPIP:  true,
					EgressVIP: egressVIP,
					Cacheable: true, // Static routing based on subnet
				}
			}
		}

		// 4. No egress node found → drop (return empty decision, will fail routing)
		return stack.RouteDecision{}
	}
}

// CalculateVIP calculates the VIP for a given subnet (subnet + 1).
func CalculateVIP(subnet *net.IPNet) net.IP {
	if subnet == nil {
		return nil
	}
	ip4 := subnet.IP.To4()
	if ip4 == nil {
		return nil
	}
	vip := make(net.IP, 4)
	copy(vip, ip4)
	vip[3] = vip[3] + 1
	return vip
}
