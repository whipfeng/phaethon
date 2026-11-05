//go:build darwin

package tun

import (
	"bufio"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"sync"

	"phaethon/util"
)

var (
	dnsMu      sync.Mutex
	dnsService string
	dnsSaved   []string
	dnsWasDHCP bool
)

// setSystemDNS redirects the active network service DNS to the given TUN IP.
func setSystemDNS(ifaceName, tunIP string) error {
	dnsMu.Lock()
	defer dnsMu.Unlock()

	service, err := findActiveNetworkService(ifaceName)
	if err != nil {
		return fmt.Errorf("find active network service: %w", err)
	}

	// Save current DNS servers
	saved, wasDHCP, err := getDNSServers(service)
	if err != nil {
		util.LogModuleWarn("dns-setup", "failed to read current dns for %s: %v", service, err)
	}

	if out, err := exec.Command("networksetup", "-setdnsservers", service, tunIP).CombinedOutput(); err != nil {
		return fmt.Errorf("set dns for %s: %v: %s", service, err, out)
	}

	dnsService = service
	dnsSaved = saved
	dnsWasDHCP = wasDHCP
	util.LogModuleInfo("dns-setup", "macOS dns for service %s set to %s", service, tunIP)
	return nil
}

// restoreSystemDNS restores the DNS configuration saved by setSystemDNS.
func restoreSystemDNS(ifaceName string) {
	dnsMu.Lock()
	defer dnsMu.Unlock()

	if dnsService == "" {
		return
	}

	var args []string
	if dnsWasDHCP || len(dnsSaved) == 0 {
		args = []string{"-setdnsservers", dnsService, "Empty"}
	} else {
		args = append([]string{"-setdnsservers", dnsService}, dnsSaved...)
	}
	if out, err := exec.Command("networksetup", args...).CombinedOutput(); err != nil {
		util.LogModuleWarn("dns-setup", "restore dns for service %s fail: %v: %s", dnsService, err, out)
	} else {
		util.LogModuleInfo("dns-setup", "macOS dns for service %s restored", dnsService)
	}

	dnsService = ""
	dnsSaved = nil
	dnsWasDHCP = false
}

// findActiveNetworkService returns the network service to configure. Among
// services whose device matches ifaceName, the first one with an active IPv4
// address wins: several services can share one device (e.g. PPPoE and Wi-Fi
// both on en0), and writing DNS to a disconnected sibling silently no-ops.
// Falls back to the first device match when none reports an IPv4 address.
func findActiveNetworkService(ifaceName string) (string, error) {
	out, err := exec.Command("networksetup", "-listnetworkserviceorder").Output()
	if err != nil {
		return "", fmt.Errorf("listnetworkserviceorder: %w", err)
	}

	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	var currentService string
	var candidates []string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "(") && !strings.HasPrefix(line, "(Hardware Port:") {
			closeIdx := strings.Index(line, ")")
			if closeIdx < 0 {
				continue
			}
			marker := line[1:closeIdx]
			if marker == "*" {
				currentService = ""
				continue
			}
			currentService = strings.TrimSpace(line[closeIdx+1:])
			continue
		}
		if strings.HasPrefix(line, "(Hardware Port:") && currentService != "" {
			if idx := strings.Index(line, "Device:"); idx >= 0 {
				device := strings.TrimSuffix(strings.TrimSpace(line[idx+len("Device:"):]), ")")
				device = strings.TrimSpace(device)
				if !strings.HasPrefix(device, "utun") && !strings.HasPrefix(device, "tun") &&
					(ifaceName == "" || strings.EqualFold(device, ifaceName)) {
					candidates = append(candidates, currentService)
				}
			}
			currentService = ""
		}
	}
	for _, svc := range candidates {
		if serviceHasIPv4(svc) {
			return svc, nil
		}
	}
	if len(candidates) > 0 {
		return candidates[0], nil
	}
	return "", fmt.Errorf("no active non-TUN network service found")
}

// serviceHasIPv4 reports whether the service currently holds an IPv4 address,
// detected by scanning -getinfo output for any parseable IPv4 token. The
// labels may be localized; the address itself is not.
func serviceHasIPv4(service string) bool {
	out, err := exec.Command("networksetup", "-getinfo", service).Output()
	if err != nil {
		return false
	}
	for _, field := range strings.Fields(string(out)) {
		if ip := net.ParseIP(strings.TrimSpace(field)); ip != nil {
			if v4 := ip.To4(); v4 != nil && !v4.Equal(net.IPv4zero) {
				return true
			}
		}
	}
	return false
}

// getDNSServers returns the configured DNS servers for the service.
// If the service uses DHCP, wasDHCP is true and saved is empty.
func getDNSServers(service string) ([]string, bool, error) {
	out, err := exec.Command("networksetup", "-getdnsservers", service).Output()
	if err != nil {
		return nil, false, err
	}
	output := string(out)
	if strings.Contains(output, "There aren't any DNS Servers") || strings.Contains(output, "not set") {
		return nil, true, nil
	}
	// networksetup output is localized: on a non-English system the DHCP
	// notice is not the English string above, so every line fails IP parsing
	// and falls through to the wasDHCP path instead of being stored as a
	// bogus "server" and written back at restore time.
	var servers []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if ip := net.ParseIP(line); ip != nil {
			servers = append(servers, line)
		}
	}
	if len(servers) == 0 {
		return nil, true, nil
	}
	return servers, false, nil
}

// dnsRegistrationIface returns the physical interface name: darwin's
// findActiveNetworkService excludes utun/tun devices from its candidates,
// so passing the TUN device name would silently no-op (6fa266e).
func dnsRegistrationIface(tunName, physicalName string) string {
	return physicalName
}
