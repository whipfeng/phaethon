//go:build linux

package tun

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

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

	// 6. Add iptables rules to allow forwarding between physical interface and
	// TUN interface. This is required for bypass gateway mode where client
	// traffic arrives on the physical interface and must be forwarded to TUN.
	// Docker's default FORWARD policy is DROP, so explicit ACCEPT rules are needed.
	if r.DefaultIfaceName != "" && r.bypassGateway {
		tunIface := r.devName
		physIface := r.DefaultIfaceName
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Allow traffic from physical to TUN (client queries going to netstack)
		if out, err := exec.CommandContext(ctx, "iptables", "-I", "FORWARD", "-i", physIface, "-o", tunIface, "-j", "ACCEPT").CombinedOutput(); err != nil {
			util.LogWarn("tun: iptables FORWARD %s->%s ACCEPT fail: %v: %s", physIface, tunIface, err, out)
		} else {
			util.LogInfo("tun: iptables FORWARD %s->%s ACCEPT added", physIface, tunIface)
		}
		// Allow traffic from TUN to physical (responses going back to clients/proxy)
		if out, err := exec.CommandContext(ctx, "iptables", "-I", "FORWARD", "-i", tunIface, "-o", physIface, "-j", "ACCEPT").CombinedOutput(); err != nil {
			util.LogWarn("tun: iptables FORWARD %s->%s ACCEPT fail: %v: %s", tunIface, physIface, err, out)
		} else {
			util.LogInfo("tun: iptables FORWARD %s->%s ACCEPT added", tunIface, physIface)
		}
		// Allow traffic from physical interface back out the same physical interface.
		// LAN client traffic arrives on physIface and must be forwarded back out
		// physIface to reach the real gateway on the same subnet.
		if out, err := exec.CommandContext(ctx, "iptables", "-I", "FORWARD", "-i", physIface, "-o", physIface, "-j", "ACCEPT").CombinedOutput(); err != nil {
			util.LogWarn("tun: iptables FORWARD %s->%s ACCEPT fail: %v: %s", physIface, physIface, err, out)
		} else {
			util.LogInfo("tun: iptables FORWARD %s->%s ACCEPT added", physIface, physIface)
		}
	}

	return nil
}

func (r *RouteManager) platformTeardown() {
	// Remove iptables FORWARD rules.
	if r.DefaultIfaceName != "" {
		tunIface := r.devName
		physIface := r.DefaultIfaceName
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Delete in reverse order of insertion
		if out, err := exec.CommandContext(ctx, "iptables", "-D", "FORWARD", "-i", physIface, "-o", physIface, "-j", "ACCEPT").CombinedOutput(); err != nil {
			util.LogWarn("tun: iptables delete FORWARD %s->%s fail: %v: %s", physIface, physIface, err, out)
		}
		if out, err := exec.CommandContext(ctx, "iptables", "-D", "FORWARD", "-i", tunIface, "-o", physIface, "-j", "ACCEPT").CombinedOutput(); err != nil {
			util.LogWarn("tun: iptables delete FORWARD %s->%s fail: %v: %s", tunIface, physIface, err, out)
		}
		if out, err := exec.CommandContext(ctx, "iptables", "-D", "FORWARD", "-i", physIface, "-o", tunIface, "-j", "ACCEPT").CombinedOutput(); err != nil {
			util.LogWarn("tun: iptables delete FORWARD %s->%s fail: %v: %s", physIface, tunIface, err, out)
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

// addMeshRoute adds a route for the mesh subnet through the TUN device.
func (e *Engine) addMeshRoute(subnet string) error {
	_, ipNet, err := net.ParseCIDR(subnet)
	if err != nil {
		return fmt.Errorf("invalid mesh subnet %s: %w", subnet, err)
	}

	link, err := netlink.LinkByName(e.routeMgr.devName)
	if err != nil {
		return fmt.Errorf("link by name %s: %w", e.routeMgr.devName, err)
	}

	rt := &netlink.Route{
		LinkIndex: link.Attrs().Index,
		Dst:       ipNet,
		Scope:     syscall.RT_SCOPE_UNIVERSE,
		Type:      syscall.RTN_UNICAST,
		Flags:     syscall.RTNH_F_ONLINK,
	}
	if err := netlink.RouteAdd(rt); err != nil {
		return fmt.Errorf("add mesh route %s: %w", subnet, err)
	}
	util.LogInfo("tun: mesh route %s -> %s (idx=%d)", subnet, e.routeMgr.devName, link.Attrs().Index)
	return nil
}

// addMeshVIPToOS adds the mesh VIP to the OS TUN interface on Linux.
func (e *Engine) addMeshVIPToOS(vip net.IP) error {
	vip4 := vip.To4()
	if vip4 == nil {
		return fmt.Errorf("only IPv4 mesh VIP supported")
	}

	link, err := netlink.LinkByName(e.routeMgr.devName)
	if err != nil {
		return fmt.Errorf("link by name %s: %w", e.routeMgr.devName, err)
	}

	addr := &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   vip4,
			Mask: net.CIDRMask(32, 32),
		},
	}
	if err := netlink.AddrAdd(link, addr); err != nil {
		if !isExist(err) {
			return fmt.Errorf("add mesh VIP %s to %s: %w", vip, e.routeMgr.devName, err)
		}
	}

	_, meshSubnet, _ := net.ParseCIDR("100.64.0.0/16")
	rt := &netlink.Route{
		LinkIndex: link.Attrs().Index,
		Dst:       meshSubnet,
		Src:       vip4,
		Scope:     syscall.RT_SCOPE_LINK,
		Type:      syscall.RTN_UNICAST,
	}
	if err := netlink.RouteAdd(rt); err != nil {
		if !isExist(err) {
			util.LogWarn("tun: add mesh source route %s src %s: %v", meshSubnet, vip4, err)
		}
	}

	util.LogInfo("tun: mesh VIP %s added to %s", vip, e.routeMgr.devName)
	return nil
}
