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
// The selector is consulted from two places (design §2.3 语义双轨):
//   - FindRoute (stack-socket outbound Connect). The decision MUST carry an
//     EgressNIC (or NeedIPIP) — otherwise fork patch #3 line 1674 falls through
//     and the SYN gets no route.
//   - handleValidatedPacket (inbound, patch #2b). The LocalDelivery flag
//     tells the IP layer to skip forwarding and AcquireAssignedAddress the
//     destination on the receiving NIC.
//
// Configure the callback to return SelectRouteResult with:
//
//	LinkNIC != 0  → route built via that NIC (direct for branch 2, IPIP for branch 3)
//	NeedIPIP=true → encapsulate inner packet in IPIP outer dst=EgressEIP
type RouteSelectorConfig struct {
	// LocalSubnet is the local node's mesh subnet (e.g., 100.1.0.0/16).
	// All addresses inside it (fakeIP/VIP/GIP/hostIP/EIP) match branch 1.
	LocalSubnet *net.IPNet

	// LocalVIP = subnet + 1. Optional defensive match (LocalSubnet subsumes it).
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
//	1. Local mesh subnet     → EgressNIC=1, LocalDelivery=true   (TUN loopback)
//	2. Mesh subnet           → SelectRoute(),                    Link NIC direct
//	3. Non-mesh advertised   → SelectRoute(),                    Link NIC + IPIP
//	4. Default fallback      → EgressNIC=1                       (TUN→OS)
//
// LocalDelivery and EgressNIC carry independent meanings (design §2.3):
//   - LocalDelivery gates handleValidatedPacket's local-delivery short-circuit
//     (patch #2b). Affects only inbound.
//   - EgressNIC is what fork patch #3 uses in FindRoute to construct a route.
//     Affects only outbound stack-socket Connect().
func NewRouteSelector(cfg *RouteSelectorConfig) stack.RouteSelector {
	return func(dst tcpip.Address) stack.RouteDecision {
		dstIP := net.IP(dst.AsSlice())

		// Branch 1: local mesh subnet (covers VIP/GIP/hostIP/fakeIP/EIP).
		// Outbound: EgressNIC=1 makes FindRoute build a TUN route; the SYN
		// loops through writeLoop → TUN → OS → NIC 1 again → handleValidatedPacket
		// (LocalDelivery=true) → Forwarder/admin listener.
		// Inbound: handleValidatedPacket sees LocalDelivery=true; if the
		// receiving NIC has a matching AddressEndpoint (GIP, VIP via AddProtocolAddress,
		// or fakeIP via promiscuous-mode temp endpoint), the packet is delivered
		// locally instead of being forwarded.
		if cfg.LocalSubnet != nil && cfg.LocalSubnet.Contains(dstIP) {
			return stack.RouteDecision{
				EgressNIC:     1,
				LocalDelivery: true,
				Cacheable:     true,
			}
		}
		// Defensive exact-match fallbacks (LocalSubnet nil case).
		if cfg.LocalGIP != (tcpip.Address{}) && dst == cfg.LocalGIP {
			return stack.RouteDecision{EgressNIC: 1, LocalDelivery: true, Cacheable: true}
		}
		if cfg.LocalVIP != (tcpip.Address{}) && dst == cfg.LocalVIP {
			return stack.RouteDecision{EgressNIC: 1, LocalDelivery: true, Cacheable: true}
		}
		if cfg.IsFakeIP != nil && cfg.IsFakeIP(dstIP) {
			return stack.RouteDecision{EgressNIC: 1, LocalDelivery: true, Cacheable: true}
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

		// Branch 4 (设计 §2.3 + 补丁 #8): 兜底 → 出栈经 TUN 走到 OS 网络栈；
		// 入栈时 handleValidatedPacket 看到 LocalDelivery=true 拦截并本地交付
		// 给 Forwarder（同 Branch 1 loopback 路径：SYN 经 NIC 1 → TUN → OS →
		// 路由回 TUN → NIC 1 → deliverPacketLocally → tcp/udp Forwarder）。
		// EgressNIC=1 让 FindRoute 在出栈（stack-socket Connect）场景下能构造
		// 真实路由。LocalDelivery=true 是兜底语义的强制要求，缺失会导致
		// forwardUnicastPacket → FindRoute(0, "", dst) 失败 → handleForwardingError
		// panic（QG 2026-10-08 复现）。
		return stack.RouteDecision{
			EgressNIC:     1,
			LocalDelivery: true,
			Cacheable:     true,
		}
	}
}

// CalculateVIP calculates the VIP for a given subnet (subnet + 1).
// (CalculateEIP lives in ipip.go; reserved addresses spec:
//  .0 network, .1 VIP, .2 hostIP, .3 GIP, .4 EIP.)
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
