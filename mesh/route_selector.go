package mesh

import (
	"net"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// RouteSelectorConfig holds configuration for the RouteSelector.
type RouteSelectorConfig struct {
	// MeshNetwork is the overall mesh network (e.g., 100.0.0.0/8).
	// Destinations inside it are left to the route table (Link NIC routes).
	MeshSubnet *net.IPNet

	// Local node's VIP (subnet + 1)
	LocalVIP tcpip.Address

	// Local node's EIP (subnet + 4). Tunnel identity only; never local-delivered.
	LocalEIP tcpip.Address

	// Local node's GIP (subnet + 3, DNS/admin listen address)
	LocalGIP tcpip.Address

	// IsFakeIP checks if an IP is a fakeIP (DNS-mapped virtual IP)
	IsFakeIP func(ip net.IP) bool

	// SelectEgressNode selects the egress node for a given destination.
	// Returns the egress node's EIP (tunnel terminator) for IPIP outer dst.
	SelectEgressNode func(dst net.IP) (tcpip.Address, bool)
}

// NewRouteSelector creates a RouteSelector function with the given configuration.
// Decision order per design §2.3. The selector only fires for the pure IP
// forwarding path (design §6.8): FindRoute(0, "", dst).
func NewRouteSelector(cfg *RouteSelectorConfig) stack.RouteSelector {
	return func(dst tcpip.Address) stack.RouteDecision {
		dstIP := net.IP(dst.AsSlice())

		// 1. fakeIP / local GIP / local VIP → local delivery
		//    (GIP = admin/DNS listen; VIP = local service address;
		//     conntrack reply traffic is already DNAT-rewritten in Prerouting
		//     and never reaches the selector)
		if cfg.IsFakeIP != nil && cfg.IsFakeIP(dstIP) {
			return stack.RouteDecision{
				LocalDelivery: true,
				Cacheable:     true,
			}
		}
		if cfg.LocalGIP != (tcpip.Address{}) && dst == cfg.LocalGIP {
			return stack.RouteDecision{LocalDelivery: true, Cacheable: true}
		}
		if cfg.LocalVIP != (tcpip.Address{}) && dst == cfg.LocalVIP {
			return stack.RouteDecision{LocalDelivery: true, Cacheable: true}
		}

		// 2. Mesh network → let route table handle it (Link NIC routes)
		if cfg.MeshSubnet != nil && cfg.MeshSubnet.Contains(dstIP) {
			return stack.RouteDecision{}
		}

		// 3. Non-mesh destination → IPIP encapsulation (outer dst = egress
		//    node EIP, the tunnel terminator identity the peer's frame-level
		//    decapsulator matches on — design §2.4/§4.5)
		if cfg.SelectEgressNode != nil {
			egressEIP, found := cfg.SelectEgressNode(dstIP)
			if found {
				return stack.RouteDecision{
					NeedIPIP:  true,
					EgressEIP: egressEIP,
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
