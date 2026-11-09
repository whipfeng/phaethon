//go:build darwin

package tun

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"

	"phaethon/util"
)

const darwinForeignSplitsPath = "data/state/darwin-foreign-splits.json"

type foreignSplitJSON struct {
	Dst     string `json:"dst"`
	GwIP    string `json:"gw_ip,omitempty"`
	GwIndex int    `json:"gw_index"`
	GwName  string `json:"gw_name,omitempty"`
}

// saveForeignSplits persists preempted third-party 0/1 routes so a crash can
// restore them (D20). Without it, SIGKILL permanently deletes another VPN's
// default routes because the snapshot only lived in memory.
func saveForeignSplits(snaps []splitRouteSnapshot) {
	if len(snaps) == 0 {
		removeForeignSplits()
		return
	}
	out := make([]foreignSplitJSON, 0, len(snaps))
	for _, s := range snaps {
		j := foreignSplitJSON{GwIndex: s.gwIndex, GwName: s.gwName}
		if s.dst != nil {
			j.Dst = s.dst.String()
		}
		if s.gwIP != nil {
			j.GwIP = s.gwIP.String()
		}
		out = append(out, j)
	}
	data, err := json.Marshal(out)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(darwinForeignSplitsPath), 0755)
	if err := os.WriteFile(darwinForeignSplitsPath, data, 0644); err != nil {
		util.LogModuleWarn("route", "persist darwin foreign splits failed: %v", err)
	}
}

func loadForeignSplits() []splitRouteSnapshot {
	data, err := os.ReadFile(darwinForeignSplitsPath)
	if err != nil {
		return nil
	}
	var in []foreignSplitJSON
	if err := json.Unmarshal(data, &in); err != nil {
		return nil
	}
	snaps := make([]splitRouteSnapshot, 0, len(in))
	for _, j := range in {
		var s splitRouteSnapshot
		if j.Dst != "" {
			if _, dst, err := net.ParseCIDR(j.Dst); err == nil {
				s.dst = dst
			}
		}
		if s.dst == nil {
			continue
		}
		if j.GwIP != "" {
			s.gwIP = net.ParseIP(j.GwIP)
		}
		s.gwIndex = j.GwIndex
		s.gwName = j.GwName
		snaps = append(snaps, s)
	}
	return snaps
}

func removeForeignSplits() {
	_ = os.Remove(darwinForeignSplitsPath)
}

// restorePersistedForeignSplits re-adds routes preempted before a crash.
func restorePersistedForeignSplits() {
	snaps := loadForeignSplits()
	if len(snaps) == 0 {
		return
	}
	for i := range snaps {
		s := &snaps[i]
		iface, err := net.InterfaceByIndex(s.gwIndex)
		if err != nil {
			util.LogModuleInfo("cleanup", "skip restore %s: interface idx=%d gone", s.dst, s.gwIndex)
			continue
		}
		if s.gwName != "" && iface.Name != s.gwName {
			util.LogModuleInfo("cleanup", "skip restore %s: idx=%d reused by %s (was %s)", s.dst, s.gwIndex, iface.Name, s.gwName)
			continue
		}
		if err := restoreSplitSnapshot(s); err != nil && !isRouteExists(err) {
			util.LogModuleWarn("cleanup", "restore foreign split %s fail: %v", s.dst, err)
		} else {
			util.LogModuleInfo("cleanup", "restored foreign split route %s after crash", s.dst)
		}
	}
	removeForeignSplits()
}
