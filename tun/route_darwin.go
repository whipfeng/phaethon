//go:build darwin

package tun

import (
	"fmt"
	"net"
	"strings"

	"phaethon/util"
)

func (r *RouteManager) platformSetup(tunIP string, prefixLen int) (err error) {
	defer func() {
		if err == nil {
			return
		}
		// Roll back our own split-tunnel routes first: a partial failure
		// (e.g. 0/1 added, 128/1 EEXIST) must not leave them behind to
		// EEXIST-block the foreign splits' restore below. ESRCH is fine.
		if r.tunIndex > 0 {
			for _, cidr := range []string{"0.0.0.0/1", "128.0.0.0/1"} {
				_, dst, _ := net.ParseCIDR(cidr)
				routeDeleteNetViaInterface(r.tunIndex, dst)
			}
		}
		r.restoreForeignSplits()
	}()
	// 0. Configure interface IP (SIOCAIFADDR) and bring it up (SIOCSIFFLAGS)
	if err := setIfaceAddr(r.devName, net.ParseIP(tunIP), prefixLen); err != nil {
		return err
	}
	if err := setIfaceUp(r.devName, true); err != nil {
		return err
	}
	// Size the overlay MTU before routes go live so the kernel derives TCP MSS
	// (MTU-40) from it. See tun.OverlayMTU for the framing budget.
	if err := setIfaceMTU(r.devName, OverlayMTU); err != nil {
		return err
	}
	util.LogModuleInfo("route", "interface %s MTU set to %d", r.devName, OverlayMTU)
	if iface, err := net.InterfaceByName(r.devName); err == nil {
		r.tunIndex = iface.Index
	} else {
		util.LogModuleWarn("route", "failed to get index for %s: %v", r.devName, err)
	}

	// 0.5 Preempt foreign split-tunnel routes (0/1, 128/1) before anything else.
	// xnu longest-prefix RTM_GET would otherwise match them both here (wrong
	// defaultRoute) and at the 0/1 add below (EEXIST). Snapshots are restored
	// at teardown while the owning interface still exists.
	r.preemptForeignSplits()

	// 1. Detect original default gateway and interface (PF_ROUTE, structured)
	gw, ifaceName, err := defaultRoute()
	if err != nil {
		util.LogModuleWarn("route", "failed to detect default gateway: %v", err)
	} else {
		r.originalGateway = gw
		r.DefaultIfaceName = ifaceName
		if iface, err := net.InterfaceByName(ifaceName); err == nil {
			r.DefaultIfaceIndex = iface.Index
		} else {
			util.LogModuleWarn("route", "interface %s not found: %v", ifaceName, err)
		}
		util.LogModuleInfo("route", "original gateway: %s (iface=%s idx=%d)", gw, ifaceName, r.DefaultIfaceIndex)
	}

	// Capture original DNS servers before TUN redirects system DNS.
	if service, err := findActiveNetworkService(r.DefaultIfaceName); err == nil {
		if servers, _, err := getDNSServers(service); err == nil {
			r.OriginalDNSServers = servers
			util.LogModuleInfo("route", "original DNS servers for %s: %v", service, servers)
		} else {
			util.LogModuleWarn("route", "failed to read current dns for %s: %v", service, err)
		}
	} else {
		util.LogModuleWarn("route", "failed to find active network service for DNS capture: %v", err)
	}

	// 2. Add exclusion routes (LAN/private subnets bypass TUN via original gateway)
	for _, exclude := range r.excludeIPs {
		if exclude == "" || exclude == tunIP || r.originalGateway == nil {
			continue
		}
		if err := r.addExclusionRoute(exclude); err != nil {
			util.LogModuleWarn("route", "add exclusion route %s fail: %v", exclude, err)
		} else {
			util.LogModuleInfo("route", "exclusion route %s -> %s", exclude, r.originalGateway)
			r.appliedExcludes = append(r.appliedExcludes, exclude)
		}
	}

	// 3. Add split-tunnel default routes scoped to the TUN interface
	if r.tunIndex > 0 {
		for _, cidr := range []string{"0.0.0.0/1", "128.0.0.0/1"} {
			_, dst, _ := net.ParseCIDR(cidr)
			if err := routeAddNetViaInterface(r.tunIndex, dst); err != nil {
				return fmt.Errorf("route add %s via %s(idx=%d): %v", cidr, r.devName, r.tunIndex, err)
			}
		}
	} else {
		return fmt.Errorf("tun interface index unknown, refusing to add default routes")
	}

	// 4. Scoped default on the physical interface. xnu scoped lookups
	// (IP_BOUND_IF) cannot resolve public destinations through the unscoped
	// default once the 0/1 hijack exists, so outbound sockets bound to the
	// physical interface fail with EHOSTUNREACH. IFSCOPE routes are invisible
	// to unscoped lookups, so user traffic is unaffected.
	if r.originalGateway != nil && r.DefaultIfaceIndex > 0 {
		if err := routeAddIfscopeDefault(r.originalGateway, r.DefaultIfaceIndex); err != nil {
			util.LogModuleWarn("route", "add ifscope default via %s fail: %v", r.originalGateway, err)
		} else {
			r.appliedIfscopeDefault = true
			util.LogModuleInfo("route", "ifscope default via %s (iface idx=%d) added", r.originalGateway, r.DefaultIfaceIndex)
		}
	}
	return nil
}

func (r *RouteManager) platformTeardown() {
	if r.tunIndex > 0 {
		for _, cidr := range []string{"0.0.0.0/1", "128.0.0.0/1"} {
			_, dst, _ := net.ParseCIDR(cidr)
			if err := routeDeleteNetViaInterface(r.tunIndex, dst); err != nil {
				util.LogModuleWarn("route", "delete %s fail: %v", cidr, err)
			}
		}
	}
	if r.appliedIfscopeDefault && r.originalGateway != nil && r.DefaultIfaceIndex > 0 {
		if err := routeDeleteIfscopeDefault(r.originalGateway, r.DefaultIfaceIndex); err != nil {
			util.LogModuleWarn("route", "delete ifscope default fail: %v", err)
		}
		r.appliedIfscopeDefault = false
	}
	r.restoreForeignSplits()
	if err := setIfaceUp(r.devName, false); err != nil {
		util.LogModuleWarn("route", "iface down %s fail: %v", r.devName, err)
	}
}

// preemptForeignSplits snapshots and removes foreign 0/1+128/1 routes so the
// TUN split-tunnel can be installed. Probe IPs are public addresses inside
// each half of the IPv4 space; a matched /1 that is not our own TUN interface
// is preempted. Failures warn and continue — the later add surfaces EEXIST.
func (r *RouteManager) preemptForeignSplits() {
	for _, probe := range []net.IP{net.IPv4(64, 0, 0, 1), net.IPv4(192, 0, 0, 1)} {
		snap, isSplit, err := routeProbeSplit(probe)
		if err != nil {
			util.LogModuleWarn("route", "probe split route via %s fail: %v", probe, err)
			continue
		}
		if !isSplit || snap.gwIndex == r.tunIndex {
			continue
		}
		if err := deleteSplitSnapshot(snap); err != nil && !isNoSuchRoute(err) {
			util.LogModuleWarn("route", "preempt foreign split %s fail: %v", snap.dst, err)
			continue
		}
		if iface, err := net.InterfaceByIndex(snap.gwIndex); err == nil {
			snap.gwName = iface.Name
		}
		r.foreignSnapshots = append(r.foreignSnapshots, *snap)
		util.LogModuleInfo("route", "preempted foreign split route %s (iface idx=%d)", snap.dst, snap.gwIndex)
	}
	saveForeignSplits(r.foreignSnapshots)
}

// restoreForeignSplits re-adds routes preempted at setup, skipping snapshots
// whose interface no longer exists (e.g. our own kill -9 leftovers pointing at
// a recycled utun).
func (r *RouteManager) restoreForeignSplits() {
	for i := range r.foreignSnapshots {
		s := &r.foreignSnapshots[i]
		iface, err := net.InterfaceByIndex(s.gwIndex)
		if err != nil {
			util.LogModuleInfo("route", "skip restore %s: interface idx=%d gone", s.dst, s.gwIndex)
			continue
		}
		if s.gwName != "" && iface.Name != s.gwName {
			util.LogModuleInfo("route", "skip restore %s: idx=%d reused by %s (was %s)", s.dst, s.gwIndex, iface.Name, s.gwName)
			continue
		}
		if err := restoreSplitSnapshot(s); err != nil && !isRouteExists(err) {
			util.LogModuleWarn("route", "restore foreign split %s fail: %v", s.dst, err)
		} else {
			util.LogModuleInfo("route", "restored foreign split route %s", s.dst)
		}
	}
	r.foreignSnapshots = nil
	removeForeignSplits()
}

// addExclusionRoute adds a host or CIDR route via the original gateway.
func (r *RouteManager) addExclusionRoute(exclude string) error {
	if strings.Contains(exclude, "/") {
		_, dst, err := net.ParseCIDR(exclude)
		if err != nil {
			return err
		}
		return routeAddNetViaGateway(dst, r.originalGateway)
	}
	ip := net.ParseIP(exclude)
	if ip == nil {
		return fmt.Errorf("bad exclusion %q", exclude)
	}
	ones := 128
	if ip.To4() != nil {
		ones = 32
	}
	return routeAddNetViaGateway(&net.IPNet{IP: ip, Mask: net.CIDRMask(ones, ones)}, r.originalGateway)
}

func (r *RouteManager) deleteExclusionRoute(exclude string) {
	if exclude == "" || r.originalGateway == nil {
		return
	}
	if strings.Contains(exclude, "/") {
		_, dst, err := net.ParseCIDR(exclude)
		if err != nil {
			return
		}
		routeDeleteNetViaGateway(dst, r.originalGateway)
		return
	}
	ip := net.ParseIP(exclude)
	if ip == nil {
		return
	}
	ones := 128
	if ip.To4() != nil {
		ones = 32
	}
	routeDeleteNetViaGateway(&net.IPNet{IP: ip, Mask: net.CIDRMask(ones, ones)}, r.originalGateway)
}

// addMeshRoute adds a route for the mesh subnet through the TUN device.
func (e *Manager) addMeshRoute(subnet string) error {
	// TODO: implement for Darwin
	util.LogModuleWarn("route", "mesh route %s NOT added: darwin route setup not implemented", subnet)
	return nil
}
