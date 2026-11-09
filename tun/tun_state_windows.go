//go:build windows

package tun

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"phaethon/util"
)

const windowsTunStatePath = "data/state/windows-tun.json"

// windowsTunState persists the physical interface state changed by TUN setup,
// so a crash can restore it on the next start (D19).
type windowsTunState struct {
	PhysicalIface string   `json:"physical_iface"`
	TunIP         string   `json:"tun_ip"`
	DNSServers    []string `json:"dns_servers,omitempty"`
	WeakHostRecv  *bool    `json:"weak_host_recv,omitempty"`
}

var windowsTunStateMu sync.Mutex

func saveWindowsTunState(st windowsTunState) {
	windowsTunStateMu.Lock()
	defer windowsTunStateMu.Unlock()
	_ = os.MkdirAll(filepath.Dir(windowsTunStatePath), 0755)
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	if err := os.WriteFile(windowsTunStatePath, data, 0644); err != nil {
		util.LogModuleWarn("route", "persist windows tun state failed: %v", err)
	}
}

func loadWindowsTunState() (windowsTunState, bool) {
	windowsTunStateMu.Lock()
	defer windowsTunStateMu.Unlock()
	data, err := os.ReadFile(windowsTunStatePath)
	if err != nil {
		return windowsTunState{}, false
	}
	var st windowsTunState
	if err := json.Unmarshal(data, &st); err != nil {
		return windowsTunState{}, false
	}
	return st, true
}

func removeWindowsTunState() {
	windowsTunStateMu.Lock()
	defer windowsTunStateMu.Unlock()
	_ = os.Remove(windowsTunStatePath)
}
