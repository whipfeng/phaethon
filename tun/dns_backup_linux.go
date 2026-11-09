//go:build linux

package tun

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"phaethon/util"
)

const linuxDNSStatePath = "data/state/linux-dns.json"

// linuxDNSState records how system DNS was changed and the pre-TUN servers,
// so both normal stop and crash recovery can restore it (D20).
type linuxDNSState struct {
	Iface   string   `json:"iface"`
	Method  string   `json:"method"` // resolved | nm
	Servers []string `json:"servers,omitempty"`
}

func saveLinuxDNSState(st linuxDNSState) {
	_ = os.MkdirAll(filepath.Dir(linuxDNSStatePath), 0755)
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	if err := os.WriteFile(linuxDNSStatePath, data, 0644); err != nil {
		util.LogModuleWarn("dns-setup", "persist linux dns state failed: %v", err)
	}
}

func loadLinuxDNSState() (linuxDNSState, bool) {
	data, err := os.ReadFile(linuxDNSStatePath)
	if err != nil {
		return linuxDNSState{}, false
	}
	var st linuxDNSState
	if err := json.Unmarshal(data, &st); err != nil || st.Iface == "" || st.Method == "" {
		return linuxDNSState{}, false
	}
	return st, true
}

func removeLinuxDNSState() {
	_ = os.Remove(linuxDNSStatePath)
}

// resolvectlDNSServers returns the current per-link DNS servers.
func resolvectlDNSServers(ctx context.Context, iface string) []string {
	out, err := exec.CommandContext(ctx, "resolvectl", "dns", iface).Output()
	if err != nil {
		return nil
	}
	var servers []string
	for _, f := range strings.Fields(string(out)) {
		if ip := net.ParseIP(f); ip != nil {
			servers = append(servers, f)
		}
	}
	return servers
}

// nmcliDNSServers returns the current IPv4 DNS list from NetworkManager.
func nmcliDNSServers(ctx context.Context, iface string) []string {
	out, err := exec.CommandContext(ctx, "nmcli", "-g", "ipv4.dns", "device", "show", iface).Output()
	if err != nil {
		return nil
	}
	var servers []string
	for _, f := range strings.FieldsFunc(string(out), func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\t'
	}) {
		if ip := net.ParseIP(f); ip != nil {
			servers = append(servers, f)
		}
	}
	return servers
}

// restoreLinuxDNSState applies a recorded state (used by crash cleanup).
func restoreLinuxDNSState(st linuxDNSState) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	switch st.Method {
	case "resolved":
		if len(st.Servers) > 0 {
			args := append([]string{"dns", st.Iface}, st.Servers...)
			if out, err := exec.CommandContext(ctx, "resolvectl", args...).CombinedOutput(); err != nil {
				util.LogModuleWarn("cleanup", "restore resolved dns for %s fail: %v: %s", st.Iface, err, out)
				return
			}
			util.LogModuleInfo("cleanup", "restored resolved dns for %s: %v", st.Iface, st.Servers)
		} else if out, err := exec.CommandContext(ctx, "resolvectl", "revert", st.Iface).CombinedOutput(); err != nil {
			util.LogModuleWarn("cleanup", "revert resolved dns for %s fail: %v: %s", st.Iface, err, out)
			return
		} else {
			util.LogModuleInfo("cleanup", "reverted resolved dns for %s", st.Iface)
		}
	case "nm":
		value := strings.Join(st.Servers, " ")
		if out, err := exec.CommandContext(ctx, "nmcli", "device", "modify", st.Iface, "ipv4.dns", value).CombinedOutput(); err != nil {
			util.LogModuleWarn("cleanup", "restore NetworkManager dns for %s fail: %v: %s", st.Iface, err, out)
			return
		}
		util.LogModuleInfo("cleanup", "restored NetworkManager dns for %s: %q", st.Iface, value)
	default:
		return
	}
	removeLinuxDNSState()
}

// restorePersistedLinuxDNS is the crash-recovery entry point.
func restorePersistedLinuxDNS() {
	st, ok := loadLinuxDNSState()
	if !ok {
		return
	}
	restoreLinuxDNSState(st)
}
