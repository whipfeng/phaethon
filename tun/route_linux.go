//go:build linux

package tun

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/vishvananda/netlink"
	"phaethon/util"
)

func (r *RouteManager) platformSetup(tunIP string, prefixLen int) error {
	link, err := netlink.LinkByName(r.devName)
	if err != nil {
		return fmt.Errorf("link by name %s: %w", r.devName, err)
	}
	r.tunIndex = link.Attrs().Index

	// 1. Set TUN interface IP
	ipNet := &net.IPNet{
		IP:   net.ParseIP(tunIP),
		Mask: net.CIDRMask(prefixLen, 32),
	}
	addr := &netlink.Addr{
		IPNet: ipNet,
	}
	if err := netlink.AddrAdd(link, addr); err != nil {
		if !isExist(err) {
			return fmt.Errorf("addr add: %w", err)
		}
	}

	// 2. Bring interface up
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("link set up: %w", err)
	}

	// 3. Detect original default gateway and interface
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		util.LogWarn("tun: failed to list routes: %v", err)
	} else {
		for _, route := range routes {
			isDefault := route.Dst == nil
			if !isDefault && route.Dst.IP.Equal(net.IPv4zero) {
				if ones, _ := route.Dst.Mask.Size(); ones == 0 {
					isDefault = true
				}
			}
			if isDefault && route.Gw != nil {
				r.originalGateway = route.Gw
				if ifaceLink, err := netlink.LinkByIndex(route.LinkIndex); err == nil {
					r.DefaultIfaceName = ifaceLink.Attrs().Name
					r.DefaultIfaceIndex = ifaceLink.Attrs().Index
				}
				util.LogInfo("tun: original gateway: %s (iface=%s idx=%d)", r.originalGateway, r.DefaultIfaceName, r.DefaultIfaceIndex)
				break
			}
		}

		// If a DNS backup from a previous run still exists, the process likely
		// crashed before restoring. Restore it first so we capture the real
		// original DNS instead of TUN-contaminated values.
		const staleBackup = "/etc/resolv.conf.phaethon.bak"
		if _, err := os.Stat(staleBackup); err == nil {
			util.LogWarn("tun: stale DNS backup found (previous crash?), restoring first")
			if origData, err := os.ReadFile(staleBackup); err == nil {
				_ = os.WriteFile("/etc/resolv.conf", origData, 0644)
			}
			_ = os.Remove(staleBackup)
		}

		// Capture original DNS servers before TUN redirects system DNS.
		if servers, err := readResolvConfNameservers(); err == nil {
			r.OriginalDNSServers = servers
			util.LogInfo("tun: original DNS servers: %v", servers)
		} else {
			util.LogWarn("tun: failed to capture original DNS servers: %v", err)
		}

		// Save and relax rp_filter on the physical interface. The split-tunnel
		// routes (0.0.0.0/1, 128.0.0.0/1) via TUN are more specific than the
		// default route, so strict rp_filter (mode 1) drops return packets
		// arriving on the physical interface because the kernel thinks they
		// should arrive via TUN. Loose mode (2) accepts them as long as any
		// route can reach the source.
		// Also enable IP forwarding and save state — all only needed for bypass
		// gateway where LAN client traffic arrives on the physical interface.
		if r.DefaultIfaceName != "" && r.bypassGateway {
			if orig, err := readRpFilter(r.DefaultIfaceName); err == nil {
				r.originalRpFilter = orig
				util.LogInfo("tun: physical %s original rp_filter: %d", r.DefaultIfaceName, orig)
			}
			if err := writeRpFilter(r.DefaultIfaceName, 2); err != nil {
				util.LogWarn("tun: set %s rp_filter=2 fail: %v", r.DefaultIfaceName, err)
			}
			if err := writeRpFilter("all", 2); err != nil {
				util.LogWarn("tun: set all rp_filter=2 fail: %v", err)
			}

			// Enable IP forwarding so the kernel forwards packets between interfaces.
			if orig, err := readIPForward(); err == nil {
				r.originalIPForward = orig
				util.LogInfo("tun: original ip_forward: %d", orig)
				if orig == 0 {
					if err := writeIPForward(1); err != nil {
						util.LogWarn("tun: enable ip_forward fail: %v", err)
					}
				}
			} else {
				util.LogWarn("tun: read ip_forward fail: %v", err)
			}

			// Save original kernel parameter values to a state file so the watchdog
			// can restore them if the main process crashes (kill -9, panic, etc.).
			saveTUNState(r.DefaultIfaceName, r.originalRpFilter, r.originalIPForward)
		}
	}

	// 4. Add exclusion routes (LAN/private subnets bypass TUN via original gateway)
	for _, exclude := range r.excludeIPs {
		if exclude == "" || exclude == tunIP || r.originalGateway == nil {
			continue
		}
		dst, err := parseExclusionLinux(exclude)
		if err != nil {
			util.LogWarn("tun: invalid exclusion %s: %v", exclude, err)
			continue
		}
		rt := &netlink.Route{
			Dst: dst,
			Gw:  r.originalGateway,
		}
		if err := netlink.RouteAdd(rt); err != nil {
			util.LogWarn("tun: add exclusion route %s fail: %v", exclude, err)
		} else {
			util.LogInfo("tun: exclusion route %s -> %s", exclude, r.originalGateway)
			r.appliedExcludes = append(r.appliedExcludes, exclude)
		}
	}

	// 5. Add split-tunnel routes (0.0.0.0/1 and 128.0.0.0/1) via TUN.
	// These are more specific than the original default route (0.0.0.0/0),
	// so they take priority. The original default route remains in place for
	// DIRECT connections that bind to the original interface.
	for _, cidr := range []string{"0.0.0.0/1", "128.0.0.0/1"} {
		_, dst, _ := net.ParseCIDR(cidr)
		rt := &netlink.Route{
			LinkIndex: link.Attrs().Index,
			Dst:       dst,
		}
		if err := netlink.RouteAdd(rt); err != nil {
			return fmt.Errorf("route add %s: %w", cidr, err)
		}
	}

	// 6. Add nftables rules to allow forwarding between physical interface and
	// TUN interface. This is required for bypass gateway mode where client
	// traffic arrives on the physical interface and must be forwarded to TUN.
	// Docker's default FORWARD policy is DROP, so explicit ACCEPT rules are needed.
	if r.DefaultIfaceName != "" && r.bypassGateway {
		tunIface := r.devName
		physIface := r.DefaultIfaceName
		
		// Allow traffic from physical to TUN (client queries going to netstack)
		if err := addNftablesForwardRule(physIface, tunIface); err != nil {
			util.LogWarn("tun: nftables FORWARD %s->%s ACCEPT fail: %v", physIface, tunIface, err)
		} else {
			util.LogInfo("tun: nftables FORWARD %s->%s ACCEPT added", physIface, tunIface)
		}
		
		// Allow traffic from TUN to physical (responses going back to clients/proxy)
		if err := addNftablesForwardRule(tunIface, physIface); err != nil {
			util.LogWarn("tun: nftables FORWARD %s->%s ACCEPT fail: %v", tunIface, physIface, err)
		} else {
			util.LogInfo("tun: nftables FORWARD %s->%s ACCEPT added", tunIface, physIface)
		}
		
		// Allow traffic from physical interface back out the same physical interface.
		// LAN client traffic arrives on physIface and must be forwarded back out
		// physIface to reach the real gateway on the same subnet.
		if err := addNftablesForwardRule(physIface, physIface); err != nil {
			util.LogWarn("tun: nftables FORWARD %s->%s ACCEPT fail: %v", physIface, physIface, err)
		} else {
			util.LogInfo("tun: nftables FORWARD %s->%s ACCEPT added", physIface, physIface)
		}
	}

	return nil
}

func (r *RouteManager) platformTeardown() {
	// Remove nftables FORWARD rules.
	if r.DefaultIfaceName != "" {
		tunIface := r.devName
		physIface := r.DefaultIfaceName
		// Delete in reverse order of insertion
		if err := delNftablesForwardRule(physIface, physIface); err != nil {
			util.LogWarn("tun: nftables delete FORWARD %s->%s fail: %v", physIface, physIface, err)
		}
		if err := delNftablesForwardRule(tunIface, physIface); err != nil {
			util.LogWarn("tun: nftables delete FORWARD %s->%s fail: %v", tunIface, physIface, err)
		}
		if err := delNftablesForwardRule(physIface, tunIface); err != nil {
			util.LogWarn("tun: nftables delete FORWARD %s->%s fail: %v", physIface, tunIface, err)
		}
	}

	// Restore rp_filter on the physical interface.
	if r.DefaultIfaceName != "" {
		if err := writeRpFilter(r.DefaultIfaceName, r.originalRpFilter); err != nil {
			util.LogWarn("tun: restore %s rp_filter=%d fail: %v", r.DefaultIfaceName, r.originalRpFilter, err)
		}
	}

	// Restore ip_forward.
	if err := writeIPForward(r.originalIPForward); err != nil {
		util.LogWarn("tun: restore ip_forward=%d fail: %v", r.originalIPForward, err)
	}

	link, err := netlink.LinkByName(r.devName)
	if err != nil {
		return
	}
	for _, cidr := range []string{"0.0.0.0/1", "128.0.0.0/1"} {
		_, dst, _ := net.ParseCIDR(cidr)
		rt := &netlink.Route{
			LinkIndex: link.Attrs().Index,
			Dst:       dst,
		}
		_ = netlink.RouteDel(rt)
	}
}

// parseExclusionLinux parses an exclusion entry into a destination IPNet.
func parseExclusionLinux(exclude string) (*net.IPNet, error) {
	if strings.Contains(exclude, "/") {
		_, ipNet, err := net.ParseCIDR(exclude)
		if err != nil {
			return nil, err
		}
		return ipNet, nil
	}
	ip := net.ParseIP(exclude)
	if ip == nil {
		return nil, fmt.Errorf("invalid IP: %s", exclude)
	}
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(32, 32)}, nil
}

func (r *RouteManager) deleteExclusionRoute(exclude string) {
	dst, err := parseExclusionLinux(exclude)
	if err != nil {
		util.LogWarn("tun: invalid exclusion to delete %s: %v", exclude, err)
		return
	}
	rt := &netlink.Route{
		Dst: dst,
		Gw:  r.originalGateway,
	}
	_ = netlink.RouteDel(rt)
}

func isExist(err error) bool {
	return errors.Is(err, syscall.EEXIST)
}

// readRpFilter reads the current rp_filter value for the given interface or
// "all"/"default" pseudo-interface from /proc/sys/net/ipv4/conf/<if>/rp_filter.
func readRpFilter(iface string) (int, error) {
	path := fmt.Sprintf("/proc/sys/net/ipv4/conf/%s/rp_filter", iface)
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, err
	}
	return v, nil
}

// writeRpFilter sets the rp_filter value for the given interface.
func writeRpFilter(iface string, value int) error {
	path := fmt.Sprintf("/proc/sys/net/ipv4/conf/%s/rp_filter", iface)
	return os.WriteFile(path, []byte(strconv.Itoa(value)), 0644)
}

func readIPForward() (int, error) {
	data, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
	if err != nil {
		return 0, err
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, err
	}
	return v, nil
}

func writeIPForward(value int) error {
	return os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte(strconv.Itoa(value)), 0644)
}

// readResolvConfNameservers returns the nameserver entries from /etc/resolv.conf.
func readResolvConfNameservers() ([]string, error) {
	f, err := os.Open("/etc/resolv.conf")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var servers []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "nameserver" {
			servers = append(servers, fields[1])
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return servers, nil
}

// addNftablesForwardRule adds a FORWARD chain rule to allow traffic from inIface to outIface.
func addNftablesForwardRule(inIface, outIface string) error {
	conn, err := nftables.New()
	if err != nil {
		return fmt.Errorf("nftables connect: %w", err)
	}

	// Get or create the filter table in the inet family (IPv4+IPv6)
	filterTable := &nftables.Table{
		Family: nftables.TableFamilyINet,
		Name:   "filter",
	}
	conn.AddTable(filterTable)

	// Get or create the FORWARD chain
	forwardChain := &nftables.Chain{
		Name:     "forward",
		Table:    filterTable,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookForward,
		Priority: nftables.ChainPriorityFilter,
	}
	conn.AddChain(forwardChain)

	// Build the rule: match input interface and output interface, then accept
	rule := &nftables.Rule{
		Table: filterTable,
		Chain: forwardChain,
		Exprs: []expr.Any{},
	}

	// Match input interface (meta iifname)
	rule.Exprs = append(rule.Exprs, &expr.Meta{
		Key:      expr.MetaKeyIIFNAME,
		Register: 1,
	})
	rule.Exprs = append(rule.Exprs, &expr.Cmp{
		Op:       expr.CmpOpEq,
		Register: 1,
		Data:     []byte(inIface + "\x00"),
	})

	// Match output interface (meta oifname)
	rule.Exprs = append(rule.Exprs, &expr.Meta{
		Key:      expr.MetaKeyOIFNAME,
		Register: 1,
	})
	rule.Exprs = append(rule.Exprs, &expr.Cmp{
		Op:       expr.CmpOpEq,
		Register: 1,
		Data:     []byte(outIface + "\x00"),
	})

	// Accept
	rule.Exprs = append(rule.Exprs, &expr.Verdict{
		Kind: expr.VerdictAccept,
	})

	conn.AddRule(rule)

	if err := conn.Flush(); err != nil {
		return fmt.Errorf("nftables flush: %w", err)
	}

	return nil
}

// delNftablesForwardRule deletes a FORWARD chain rule for the specified interfaces.
func delNftablesForwardRule(inIface, outIface string) error {
	conn, err := nftables.New()
	if err != nil {
		return fmt.Errorf("nftables connect: %w", err)
	}

	// Get the filter table
	filterTable := &nftables.Table{
		Family: nftables.TableFamilyINet,
		Name:   "filter",
	}

	// Get the FORWARD chain
	forwardChain := &nftables.Chain{
		Name:  "forward",
		Table: filterTable,
	}

	// List all rules in the chain
	rules, err := conn.GetRules(filterTable, forwardChain)
	if err != nil {
		return fmt.Errorf("get rules: %w", err)
	}

	// Find and delete the matching rule
	for _, rule := range rules {
		if matchesForwardRule(rule, inIface, outIface) {
			conn.DelRule(rule)
			if err := conn.Flush(); err != nil {
				return fmt.Errorf("delete rule: %w", err)
			}
			return nil
		}
	}

	return fmt.Errorf("rule not found")
}

// matchesForwardRule checks if a rule matches the specified input/output interfaces.
func matchesForwardRule(rule *nftables.Rule, inIface, outIface string) bool {
	// Simple heuristic: check if the rule expressions contain the interface names
	// This is a simplified check - in production you'd want to parse the expressions properly
	exprsData := fmt.Sprintf("%v", rule.Exprs)
	return strings.Contains(exprsData, inIface) && strings.Contains(exprsData, outIface)
}

// tunStateFile is the path where the main process saves kernel parameter values
// before modifying them. The watchdog reads this file to restore the original
// values if the main process crashes.
const tunStateFile = "/var/run/phaethon-tun.state"

// tunState holds the original kernel parameter values before TUN setup.
type tunState struct {
	IfaceName string `json:"ifaceName"`
	RpFilter  int    `json:"rpFilter"`
	IPForward int    `json:"ipForward"`
}

// saveTUNState writes the original kernel parameter values to a state file.
// The watchdog reads this file to restore the values if the main process crashes.
func saveTUNState(ifaceName string, rpFilter, ipForward int) {
	state := tunState{
		IfaceName: ifaceName,
		RpFilter:  rpFilter,
		IPForward: ipForward,
	}
	data, err := json.Marshal(state)
	if err != nil {
		util.LogWarn("tun: marshal state fail: %v", err)
		return
	}
	if err := os.WriteFile(tunStateFile, data, 0644); err != nil {
		util.LogWarn("tun: save state fail: %v", err)
	} else {
		util.LogInfo("tun: saved state to %s: iface=%s rp_filter=%d ip_forward=%d", tunStateFile, ifaceName, rpFilter, ipForward)
	}
}
