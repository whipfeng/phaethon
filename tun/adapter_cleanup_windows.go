//go:build windows

package tun

import "phaethon/util"

// deleteTUNAdapter removes the persisted Wintun adapter after StopTUN.
// Without this the adapter survives process exit, keeping routes and DNS
// bound to a dead interface (D19).
func deleteTUNAdapter(name string) {
	if name == "" {
		return
	}
	if err := removeAdapterAPI(name); err != nil {
		util.LogModuleDebug("tun", "tun: remove adapter %s after stop: %v", name, err)
	}
}
