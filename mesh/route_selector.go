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

	// LocalVIP = subnet + 1. Required: it is the sole discriminator that keeps
	// Forwarder / DNS-hijacker replies egressing via NIC 1 instead of being
	// looped back in-stack (see NewRouteSelector). Must match the address the
	// Input-chain SNAT rewrites inbound client sources to.
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
//	1. Local mesh subnet     → EgressNIC=1, LocalDelivery=true,
//	                           LocalLoopback=(dst != VIP)   (in-stack loopback)
//	2. Mesh subnet           → SelectRoute(),               Link NIC direct
//	3. Non-mesh advertised   → SelectRoute(),               Link NIC + IPIP
//	4. Default fallback      → EgressNIC=1, LocalDelivery=true,
//	                           LocalLoopback=true           (in-stack loopback)
//
// The three flags carry independent meanings (design §2.3 / §6.13):
//   - LocalDelivery gates handleValidatedPacket's local-delivery short-circuit
//     (patch #2b). Affects only inbound.
//   - LocalLoopback gates FindRoute's Route.Loop=PacketLoop override
//     (patch #10). Affects only outbound stack-socket writes: the packet is
//     delivered back into the stack instead of being written to the egress
//     NIC, so local delivery no longer depends on a TUN device existing.
//   - EgressNIC is what fork patch #3 uses in FindRoute to construct a route.
//     Still required for looped-back packets — FindRoute needs a NIC to obtain
//     an address endpoint from, even though the route never writes to it.
//
// Why branch 1 excludes VIP: the Input-chain SNAT (netstack.go, rule
// InputInterface=="tun" → src rewritten to VIP) normalizes every
// NIC-inbound locally-delivered client to VIP, so the TCP Forwarder and the
// DNS hijacker only ever see src=VIP and address their replies to VIP. Those
// replies must leave via NIC 1 and be reverse-translated by conntrack at
// Postrouting; looping them back would break TUN clients and the bypass
// gateway. Confirmed on QG: every forwarder connection logs remote=100.0.0.1
// and every hijacker query logs from=100.0.0.1. No phaethon-owned socket ever
// dials VIP, so the exclusion cannot misfire. Changing that SNAT rule
// therefore requires revisiting this branch.
func NewRouteSelector(cfg *RouteSelectorConfig) stack.RouteSelector {
	return func(dst tcpip.Address) stack.RouteDecision {
		dstIP := net.IP(dst.AsSlice())

		// Branch 1: local mesh subnet (covers VIP/GIP/hostIP/fakeIP/EIP).
		// Outbound: LocalLoopback makes FindRoute set Route.Loop=PacketLoop, so
		// writePacketPostRouting calls handleLocalPacket and returns before any
		// NIC write → handleValidatedPacket (LocalDelivery=true) →
		// deliverPacketLocally → Forwarder/admin listener. VIP is excluded: it
		// is the reply destination for NIC-inbound clients (see above).
		// Inbound: handleValidatedPacket sees LocalDelivery=true; if the
		// receiving NIC has a matching AddressEndpoint (GIP, VIP via AddProtocolAddress,
		// or fakeIP via promiscuous-mode temp endpoint), the packet is delivered
		// locally instead of being forwarded.
		if cfg.LocalSubnet != nil && cfg.LocalSubnet.Contains(dstIP) {
			return stack.RouteDecision{
				EgressNIC:     1,
				LocalDelivery: true,
				LocalLoopback: dst != cfg.LocalVIP,
				Cacheable:     true,
			}
		}
		// Defensive exact-match fallbacks (LocalSubnet nil case).
		if cfg.LocalGIP != (tcpip.Address{}) && dst == cfg.LocalGIP {
			return stack.RouteDecision{EgressNIC: 1, LocalDelivery: true, LocalLoopback: true, Cacheable: true}
		}
		if cfg.LocalVIP != (tcpip.Address{}) && dst == cfg.LocalVIP {
			return stack.RouteDecision{EgressNIC: 1, LocalDelivery: true, Cacheable: true}
		}
		if cfg.IsFakeIP != nil && cfg.IsFakeIP(dstIP) {
			return stack.RouteDecision{EgressNIC: 1, LocalDelivery: true, LocalLoopback: true, Cacheable: true}
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

		// Branch 4 (设计 §2.3 + 补丁 #8/#10): 兜底。
		// 入栈：handleValidatedPacket 看到 LocalDelivery=true 拦截并本地交付给
		// Forwarder —— 这是 TUN 抓到的 OS 流量与旁路网关流量的代理入口。
		// LocalDelivery=true 是强制要求，缺失会导致 forwardUnicastPacket →
		// FindRoute(0, "", dst) 失败 → handleForwardingError panic
		// （QG 2026-10-08 复现）。
		// 出栈：LocalLoopback=true 让包在栈内环回给 Forwarder，不写 NIC，
		// 因此不依赖 TUN 设备存在（补丁 #10）。出站命中本分支的只有 phaethon
		// 自有 socket（NetDial / 栈内 DNS）：Forwarder 与 DNS 劫持器的回包
		// dst 是 VIP，落 Branch 1。
		// EgressNIC=1 仍需保留：FindRoute 要靠它取 address endpoint 构造 route，
		// 即使该 route 最终不写 NIC。
		return stack.RouteDecision{
			EgressNIC:     1,
			LocalDelivery: true,
			LocalLoopback: true,
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
