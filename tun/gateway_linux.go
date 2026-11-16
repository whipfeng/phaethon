//go:build linux

package tun

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"phaethon/util"
)

// gatewayNAT completes the bypass-gateway host setup that already exists in
// route_linux.go: that code inserts the phys→tun / tun→phys / phys→phys
// FORWARD ACCEPT tuples, and this type (a) keeps those tuples present when
// Docker flushes/reorders FORWARD, and (b) adds the one missing piece, the
// POSTROUTING MASQUERADE for the LAN clients served by the DHCP server.
//
// The gVisor NAT table in mesh/netstack.go only covers packets entering the
// TUN NIC; LAN clients arrive on the physical interface and are forwarded by
// the host kernel, which needs its own NAT rule.
type gatewayNAT struct {
	lanIface      string
	upstreamIface string
	tunIface      string
	lanCIDR       string

	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}
}

// setupGatewayNAT starts the reconciler for the existing FORWARD tuples and
// installs the LAN MASQUERADE. Returns (nil, nil) when the LAN interface has
// no IPv4 address to serve.
func setupGatewayNAT(lanIface, upstreamIface, tunIface string) (*gatewayNAT, error) {
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
		tunIface:      tunIface,
		lanCIDR:       cidr,
		stopCh:        make(chan struct{}),
		doneCh:        make(chan struct{}),
	}
	if err := g.install(); err != nil {
		return nil, err
	}
	go g.reconcileLoop()
	util.LogModuleInfo("tun", "bypass gateway: LAN %s via %s, MASQUERADE installed", cidr, upstreamIface)
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

func iptablesCtx(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	path, err := exec.LookPath("iptables")
	if err != nil {
		return fmt.Errorf("iptables not found: %w", err)
	}
	out, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("iptables %s: %v (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func ruleExists(args ...string) bool { return iptablesCtx(args...) == nil }

// ensureForwardTuple mirrors the tuples installed by route_linux.go step 6.
// They are re-inserted only when missing (Docker restart/flush).
func (g *gatewayNAT) ensureForwardTuple() error {
	tuples := [][2]string{
		{g.upstreamIface, g.tunIface},
		{g.tunIface, g.upstreamIface},
		{g.upstreamIface, g.upstreamIface},
	}
	for _, t := range tuples {
		if ruleExists("-C", "FORWARD", "-i", t[0], "-o", t[1], "-j", "ACCEPT") {
			continue
		}
		if err := iptablesCtx("-I", "FORWARD", "1", "-i", t[0], "-o", t[1], "-j", "ACCEPT"); err != nil {
			return err
		}
	}
	return nil
}

// ensureMasquerade adds the LAN → upstream MASQUERADE, the missing piece
// relative to route_linux.go (which only had FORWARD rules).
func (g *gatewayNAT) ensureMasquerade() error {
	args := []string{"-t", "nat", "-C", "POSTROUTING", "-s", g.lanCIDR, "-o", g.upstreamIface, "-j", "MASQUERADE"}
	if ruleExists(args...) {
		return nil
	}
	add := []string{"-t", "nat", "-I", "POSTROUTING", "1", "-s", g.lanCIDR, "-o", g.upstreamIface, "-j", "MASQUERADE"}
	return iptablesCtx(add...)
}

func (g *gatewayNAT) install() error {
	if err := g.ensureForwardTuple(); err != nil {
		return err
	}
	if err := g.ensureMasquerade(); err != nil {
		return err
	}
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
			if err := g.install(); err != nil {
				util.LogModuleWarn("tun", "bypass gateway reconcile: %v", err)
			}
		}
	}
}

// Stop removes the MASQUERADE rule. The FORWARD tuples are owned and removed
// by route_linux.go's teardown (single owner, no double-free).
func (g *gatewayNAT) Stop() {
	if g == nil {
		return
	}
	g.stopOnce.Do(func() {
		close(g.stopCh)
		<-g.doneCh
		for i := 0; i < 16; i++ {
			if iptablesCtx("-t", "nat", "-D", "POSTROUTING", "-s", g.lanCIDR, "-o", g.upstreamIface, "-j", "MASQUERADE") != nil {
				break
			}
		}
	})
}
