//go:build linux

package tun

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"phaethon/util"
)

// gatewayNAT makes the host a real bypass gateway for the LAN clients served
// by the DHCP server: kernel forwarding plus a scoped MASQUERADE for the LAN
// subnet. The gVisor NAT table in mesh/netstack.go only covers packets that
// enter the TUN NIC; LAN clients arrive on the physical interface and are
// forwarded by the host kernel, which needs its own FORWARD/NAT rules.
//
// Docker sets `iptables -P FORWARD DROP` and may flush/reorder rules when it
// restarts, so the jumps are inserted at position 1 and a reconciler re-adds
// them if they disappear. Only the LAN CIDR is accepted/NATed; every other
// packet still follows the host's policy.
type gatewayNAT struct {
	lanIface      string
	upstreamIface string
	lanCIDR       string

	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}
}

const (
	gwFwdChain = "PHAETHON_GW_FWD"
	gwNatChain = "PHAETHON_GW_NAT"
)

// setupGatewayNAT installs the gateway rules and starts the reconciler.
// Returns (nil, nil) when the LAN interface has no IPv4 address to serve.
func setupGatewayNAT(lanIface, upstreamIface string) (*gatewayNAT, error) {
	cidr, err := ifaceIPv4CIDR(lanIface)
	if err != nil {
		return nil, err
	}
	if upstreamIface == "" {
		upstreamIface = lanIface
	}
	g := &gatewayNAT{
		lanIface:      lanIface,
		upstreamIface: upstreamIface,
		lanCIDR:       cidr,
		stopCh:        make(chan struct{}),
		doneCh:        make(chan struct{}),
	}
	if err := g.install(); err != nil {
		return nil, err
	}
	go g.reconcileLoop()
	util.LogModuleInfo("tun", "bypass gateway: LAN %s via %s, NAT chain %s", cidr, upstreamIface, gwNatChain)
	return g, nil
}

func ifaceIPv4CIDR(name string) (string, error) {
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return "", fmt.Errorf("interface %s: %w", name, err)
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		v4 := ipnet.IP.To4()
		if v4 == nil {
			continue
		}
		ones, bits := ipnet.Mask.Size()
		if bits != 32 || ones <= 0 {
			continue
		}
		return (&net.IPNet{IP: v4.Mask(ipnet.Mask), Mask: ipnet.Mask}).String(), nil
	}
	return "", fmt.Errorf("interface %s has no IPv4 address", name)
}

func iptables(args ...string) error {
	path, err := exec.LookPath("iptables")
	if err != nil {
		return fmt.Errorf("iptables not found: %w", err)
	}
	out, err := exec.Command(path, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("iptables %s: %v (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func jumpExists(table, parent, chain string) bool {
	args := []string{}
	if table != "" && table != "filter" {
		args = append(args, "-t", table)
	}
	args = append(args, "-C", parent, "-j", chain)
	return iptables(args...) == nil
}

// install creates/flushes the app chains and re-inserts the jumps at position 1.
func (g *gatewayNAT) install() error {
	// NAT: LAN -> upstream MASQUERADE.
	_ = iptables("-t", "nat", "-N", gwNatChain)
	if err := iptables("-t", "nat", "-F", gwNatChain); err != nil {
		return err
	}
	if err := iptables("-t", "nat", "-A", gwNatChain, "-s", g.lanCIDR, "-o", g.upstreamIface, "-j", "MASQUERADE"); err != nil {
		return err
	}
	if !jumpExists("nat", "POSTROUTING", gwNatChain) {
		if err := iptables("-t", "nat", "-I", "POSTROUTING", "1", "-j", gwNatChain); err != nil {
			return err
		}
	}

	// FORWARD: accept LAN clients and their established return traffic before
	// Docker's FORWARD policy.
	_ = iptables("-N", gwFwdChain)
	if err := iptables("-F", gwFwdChain); err != nil {
		return err
	}
	if err := iptables("-A", gwFwdChain, "-s", g.lanCIDR, "-j", "ACCEPT"); err != nil {
		return err
	}
	if err := iptables("-A", gwFwdChain, "-d", g.lanCIDR, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"); err != nil {
		return err
	}
	if !jumpExists("filter", "FORWARD", gwFwdChain) {
		if err := iptables("-I", "FORWARD", "1", "-j", gwFwdChain); err != nil {
			return err
		}
	}

	// The route manager already enables this for bypass gateway; re-assert it
	// because a sysctl reload or another net daemon may reset it.
	_ = os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644)
	return nil
}

func (g *gatewayNAT) reconcileLoop() {
	defer close(g.doneCh)
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-g.stopCh:
			return
		case <-tick.C:
			// Docker restarts can flush/reorder rules; only rebuild when a
			// jump or a sentinel rule is missing (avoid flushing live traffic).
			if !jumpExists("filter", "FORWARD", gwFwdChain) ||
				!jumpExists("nat", "POSTROUTING", gwNatChain) ||
				iptables("-C", gwFwdChain, "-s", g.lanCIDR, "-j", "ACCEPT") != nil {
				if err := g.install(); err != nil {
					util.LogModuleWarn("tun", "bypass gateway reconcile: %v", err)
				} else {
					util.LogModuleInfo("tun", "bypass gateway rules restored")
				}
			}
		}
	}
}

// Stop removes the jumps and chains. Safe to call multiple times.
func (g *gatewayNAT) Stop() {
	if g == nil {
		return
	}
	g.stopOnce.Do(func() {
		close(g.stopCh)
		<-g.doneCh
		for i := 0; i < 16; i++ {
			if iptables("-D", "FORWARD", "-j", gwFwdChain) != nil {
				break
			}
		}
		for i := 0; i < 16; i++ {
			if iptables("-t", "nat", "-D", "POSTROUTING", "-j", gwNatChain) != nil {
				break
			}
		}
		_ = iptables("-F", gwFwdChain)
		_ = iptables("-X", gwFwdChain)
		_ = iptables("-t", "nat", "-F", gwNatChain)
		_ = iptables("-t", "nat", "-X", gwNatChain)
	})
}
