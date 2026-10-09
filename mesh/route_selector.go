package mesh

import (
	"net"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// SelectRouteResult is the decision returned by RouteSelectorConfig.SelectRoute.
// It is consulted from the RouteSelector for branches 2 (mesh subnet, no IPIP)
// and 3 (non-mesh advertised route, IPIP encapsulated).
//
// NeedIPIP distinguishes the two branches:
//   - false → branch 2 (mesh target). Cacheable=false in the resulting
//     RouteSelector decision because topology changes can shift the next-hop
//     Link NIC (Dijkstra recompute).
//   - true  → branch 3 (advertised non-mesh). Cacheable=true because the
//     egress node selection is static per prefix.
type SelectRouteResult struct {
	LinkNIC   tcpip.NICID
	EgressEIP net.IP // only meaningful when NeedIPIP=true
	NeedIPIP  bool
}

// SelectRouteFunc decides how to route a non-local destination.
//
// Returns ok=true with a non-zero LinkNIC for both branches 2 (mesh) and
// 3 (non-mesh advertised). ok=false means "no opinion" — RouteSelector then
// falls through to branch 4 (default NIC 1 fallback).
type SelectRouteFunc func(dst net.IP) (SelectRouteResult, bool)

// RouteSelectorConfig holds the configuration for NewRouteSelector.
//
// LocalStack is consumed by both FindRoute and handleValidatedPacket: it marks
// destinations handled by the local proxy/service stack. EgressNIC selects the
// NIC for ordinary forwarding; conntrack return packets instead use their
// recorded OutputNICName and bypass this destination-only policy.
//
// Configure the callback to return SelectRouteResult with:
//
//	LinkNIC != 0  → route built via that NIC (direct for branch 2, IPIP for branch 3)
//	NeedIPIP=true → encapsulate inner packet in IPIP outer dst=EgressEIP
type RouteSelectorConfig struct {
	// LocalSubnet is the local node's mesh subnet (e.g., 100.1.0.0/16).
	// All addresses inside it (fakeIP/VIP/GIP/hostIP/EIP) match branch 1.
	LocalSubnet *net.IPNet

	// LocalVIP = subnet + 1. It is a conntrack/SNAT transit address, not a
	// local service address, so RouteSelector marks it LocalStack=false. It must
	// match the address the Input-chain SNAT rewrites inbound client sources to.
	LocalVIP tcpip.Address
	// LocalGIP = subnet + 3 (DNS/admin listen). Optional defensive match.
	LocalGIP tcpip.Address

	// IsFakeIP checks if an IP is a fakeIP allocated from the local pool
	// (excludes reserved infrastructure addresses). Defensive match —
	// LocalSubnet.Contains already covers the alloc range.
	IsFakeIP func(ip net.IP) bool

	// SelectRoute picks the egress path for non-local destinations
	// (branches 2 + 3). It is the caller's job to encapsulate topology
	// selection (Dijkstra next-hop, advertised-route lookup) into this
	// callback.
	SelectRoute SelectRouteFunc
}

// NewRouteSelector creates a RouteSelector per design §2.3 (4 branches).
//
// Branch resolution:
//
//  1. Local mesh subnet     → EgressNIC=1, LocalStack=(dst != VIP)
//  2. Mesh subnet           → SelectRoute(), Link NIC direct
//  3. Non-mesh advertised   → SelectRoute(), Link NIC + IPIP
//  4. Default fallback      → EgressNIC=1, LocalStack=true
//
// LocalStack unifies local delivery and in-stack loopback. EgressNIC remains
// necessary to construct routes even for LocalStack destinations. VIP is
// excluded because it is only the Input-SNAT/conntrack transit address: a
// packet still addressed to VIP must leave NIC 1. Conntrack return packets
// carry OutputNICName and bypass this destination-only selector entirely.
func NewRouteSelector(cfg *RouteSelectorConfig) stack.RouteSelector {
	return func(dst tcpip.Address) stack.RouteDecision {
		dstIP := net.IP(dst.AsSlice())

		// Branch 1: local mesh subnet. VIP is a conntrack transit address;
		// GIP, hostIP, EIP and fakeIP are local stack destinations.
		if cfg.LocalSubnet != nil && cfg.LocalSubnet.Contains(dstIP) {
			return stack.RouteDecision{
				EgressNIC:  1,
				LocalStack: dst != cfg.LocalVIP,
				Cacheable:  true,
			}
		}
		// Defensive exact-match fallbacks (LocalSubnet nil case).
		if cfg.LocalGIP != (tcpip.Address{}) && dst == cfg.LocalGIP {
			return stack.RouteDecision{EgressNIC: 1, LocalStack: true, Cacheable: true}
		}
		if cfg.LocalVIP != (tcpip.Address{}) && dst == cfg.LocalVIP {
			return stack.RouteDecision{EgressNIC: 1, Cacheable: true}
		}
		if cfg.IsFakeIP != nil && cfg.IsFakeIP(dstIP) {
			return stack.RouteDecision{EgressNIC: 1, LocalStack: true, Cacheable: true}
		}

		// Branches 2 & 3: SelectRoute covers both mesh and non-mesh advertised.
		if cfg.SelectRoute != nil {
			if r, ok := cfg.SelectRoute(dstIP); ok && r.LinkNIC != 0 {
				var egressEIP tcpip.Address
				if r.NeedIPIP && r.EgressEIP != nil {
					egressEIP = tcpip.AddrFrom4Slice(r.EgressEIP)
				}
				return stack.RouteDecision{
					EgressNIC: r.LinkNIC,
					NeedIPIP:  r.NeedIPIP,
					EgressEIP: egressEIP,
					Cacheable: r.NeedIPIP, // IPIP egress is static per prefix; mesh is dynamic
				}
			}
		}

		// Branch 4: unknown destinations are local proxy entries. Conntrack
		// returns carry OutputNICName and bypass this branch in the receive path.
		return stack.RouteDecision{
			EgressNIC:  1,
			LocalStack: true,
			Cacheable:  true,
		}
	}
}

// CalculateVIP calculates the VIP for a given subnet (subnet + 1).
// (CalculateEIP lives in ipip.go; reserved addresses spec:
//
//	.0 network, .1 VIP, .2 hostIP, .3 GIP, .4 EIP.)
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
