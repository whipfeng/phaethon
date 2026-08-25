//go:build darwin

package tun

import (
	"bufio"
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"phaethon/util"
)

var (
	dnsMu        sync.Mutex
	dnsService   string
	dnsSaved     []string
	dnsWasDHCP   bool
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
		util.LogWarn("tun: failed to read current dns for %s: %v", service, err)
	}

	if out, err := exec.Command("networksetup", "-setdnsservers", service, tunIP).CombinedOutput(); err != nil {
		return fmt.Errorf("set dns for %s: %v: %s", service, err, out)
	}

	dnsService = service
	dnsSaved = saved
	dnsWasDHCP = wasDHCP
	util.LogInfo("tun: macOS dns for service %s set to %s", service, tunIP)
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
		util.LogWarn("tun: restore dns for service %s fail: %v: %s", dnsService, err, out)
	} else {
		util.LogInfo("tun: macOS dns for service %s restored", dnsService)
	}

	dnsService = ""
	dnsSaved = nil
	dnsWasDHCP = false
}

// findActiveNetworkService returns the network service to configure.
// If ifaceName is provided, it prefers the service whose device matches it;
// otherwise it returns the first enabled non-TUN service.
func findActiveNetworkService(ifaceName string) (string, error) {
	out, err := exec.Command("networksetup", "-listnetworkserviceorder").Output()
	if err != nil {
		return "", fmt.Errorf("listnetworkserviceorder: %w", err)
	}

	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	var currentService string
	var fallback string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "(") {
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
				if !strings.HasPrefix(device, "utun") && !strings.HasPrefix(device, "tun") {
					if fallback == "" {
						fallback = currentService
					}
					if ifaceName != "" && strings.EqualFold(device, ifaceName) {
						return currentService, nil
					}
				}
			}
			currentService = ""
		}
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", fmt.Errorf("no active non-TUN network service found")
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
	var servers []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		servers = append(servers, line)
	}
	return servers, false, nil
}
