//go:build darwin

package tun

import (
	"fmt"
	"net"
	"os/exec"
	"strings"

	"phaethon/util"
)

func (r *RouteManager) platformSetup(tunIP string, prefixLen int) error {
	// 1. Detect original default gateway and interface
	gw, ifaceName, err := getDefaultGatewayDarwin()
	if err != nil {
		util.LogWarn("tun: failed to detect default gateway: %v", err)
	} else {
		r.originalGateway = gw
		r.DefaultIfaceName = ifaceName
		if iface, err := net.InterfaceByName(ifaceName); err == nil {
			r.DefaultIfaceIndex = iface.Index
		} else {
			util.LogWarn("tun: interface %s not found: %v", ifaceName, err)
		}
		util.LogInfo("tun: original gateway: %s (iface=%s idx=%d)", gw, ifaceName, r.DefaultIfaceIndex)
	}

	// 2. Add exclusion routes
	for _, exclude := range r.excludeIPs {
		if exclude == "" || exclude == tunIP || r.originalGateway == nil {
			continue
		}
		if err := r.addExclusionRoute(exclude); err != nil {
			util.LogWarn("tun: add exclusion route %s fail: %v", exclude, err)
		} else {
			util.LogInfo("tun: exclusion route %s -> %s", exclude, r.originalGateway)
			r.appliedExcludes = append(r.appliedExcludes, exclude)
		}
	}

	// 3. Add split-tunnel default routes
	cmd := exec.Command("route", "-n", "add", "-net", "0.0.0.0/1", "-interface", r.devName)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("route add 0/1: %v, %s", err, out)
	}
	cmd = exec.Command("route", "-n", "add", "-net", "128.0.0.0/1", "-interface", r.devName)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("route add 128/1: %v, %s", err, out)
	}
	return nil
}

func (r *RouteManager) platformTeardown() {
	exec.Command("route", "-n", "delete", "-net", "0.0.0.0/1").Run()
	exec.Command("route", "-n", "delete", "-net", "128.0.0.0/1").Run()
}

// addExclusionRoute adds a host or CIDR route via the original gateway.
func (r *RouteManager) addExclusionRoute(exclude string) error {
	if strings.Contains(exclude, "/") {
		cmd := exec.Command("route", "-n", "add", "-net", exclude, r.originalGateway.String())
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%v: %s", err, out)
		}
		return nil
	}
	cmd := exec.Command("route", "-n", "add", "-host", exclude, r.originalGateway.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}

func (r *RouteManager) deleteExclusionRoute(exclude string) {
	if exclude == "" || r.originalGateway == nil {
		return
	}
	if strings.Contains(exclude, "/") {
		exec.Command("route", "-n", "delete", "-net", exclude, r.originalGateway.String()).Run()
		return
	}
	exec.Command("route", "-n", "delete", "-host", exclude, r.originalGateway.String()).Run()
}

func getDefaultGatewayDarwin() (net.IP, string, error) {
	out, err := exec.Command("netstat", "-rn").Output()
	if err != nil {
		return nil, "", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 4 && fields[0] == "default" {
			gw := net.ParseIP(fields[1])
			if gw != nil {
				return gw, fields[3], nil
			}
		}
	}
	return nil, "", fmt.Errorf("no default gateway found")
}
