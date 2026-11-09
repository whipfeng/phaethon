//go:build darwin

package tun

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"

	"phaethon/util"
)

const darwinDNSBackupPath = "data/state/darwin-dns.json"

type darwinDNSBackup struct {
	Service string   `json:"service"`
	Servers []string `json:"servers"`
	WasDHCP bool     `json:"was_dhcp"`
}

// saveDarwinDNSBackup persists the pre-TUN DNS configuration so a crash can be
// recovered on the next start (D20). The in-memory copy alone is lost on
// SIGKILL, leaving the whole machine pointing at a dead utun address.
func saveDarwinDNSBackup(service string, servers []string, wasDHCP bool) {
	if service == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(darwinDNSBackupPath), 0755)
	data, err := json.Marshal(darwinDNSBackup{Service: service, Servers: servers, WasDHCP: wasDHCP})
	if err != nil {
		return
	}
	if err := os.WriteFile(darwinDNSBackupPath, data, 0644); err != nil {
		util.LogModuleWarn("dns-setup", "persist darwin dns backup failed: %v", err)
	}
}

func loadDarwinDNSBackup() (darwinDNSBackup, bool) {
	data, err := os.ReadFile(darwinDNSBackupPath)
	if err != nil {
		return darwinDNSBackup{}, false
	}
	var b darwinDNSBackup
	if err := json.Unmarshal(data, &b); err != nil || b.Service == "" {
		return darwinDNSBackup{}, false
	}
	return b, true
}

func removeDarwinDNSBackup() {
	_ = os.Remove(darwinDNSBackupPath)
}

// restoreDarwinDNSBackup restores the persisted backup, if any. Called by
// CleanupResidual before TUN starts.
func restoreDarwinDNSBackup() {
	b, ok := loadDarwinDNSBackup()
	if !ok {
		return
	}
	var args []string
	if b.WasDHCP || len(b.Servers) == 0 {
		args = []string{"-setdnsservers", b.Service, "Empty"}
	} else {
		args = append([]string{"-setdnsservers", b.Service}, b.Servers...)
	}
	if out, err := exec.Command("networksetup", args...).CombinedOutput(); err != nil {
		util.LogModuleWarn("dns-setup", "restore persisted dns for %s fail: %v: %s", b.Service, err, out)
		return
	}
	util.LogModuleInfo("dns-setup", "restored persisted dns for service %s after crash", b.Service)
	removeDarwinDNSBackup()
}
