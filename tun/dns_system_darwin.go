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
func setSystemDNS(devName, tunIP string) error {
	dnsMu.Lock()
	defer dnsMu.Unlock()

	service, err := findActiveNetworkService(devName)
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
func restoreSystemDNS(devName string) {
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
// If devName is a utun/tun device, it tries to find the first non-TUN enabled service.
func findActiveNetworkService(devName string) (string, error) {
	out, err := exec.Command("networksetup", "-listnetworkserviceorder").Output()
	if err != nil {
		return "", fmt.Errorf("listnetworkserviceorder: %w", err)
	}

	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	var currentService string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		// Service line: "(1) Wi-Fi" or "(*) Bluetooth PAN" (disabled)
		if strings.HasPrefix(line, "(") {
			closeIdx := strings.Index(line, ")")
			if closeIdx < 0 {
				continue
			}
			marker := line[1:closeIdx]
			if marker == "*" {
				currentService = "" // disabled service
				continue
			}
			currentService = strings.TrimSpace(line[closeIdx+1:])
			continue
		}
		// Hardware line: "(Hardware Port: Wi-Fi, Device: en0)"
		if strings.HasPrefix(line, "(Hardware Port:") && currentService != "" {
			if idx := strings.Index(line, "Device:"); idx >= 0 {
				device := strings.TrimSuffix(strings.TrimSpace(line[idx+len("Device:"):]), ")")
				device = strings.TrimSpace(device)
				if !strings.HasPrefix(device, "utun") && !strings.HasPrefix(device, "tun") {
					return currentService, nil
				}
			}
		}
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
