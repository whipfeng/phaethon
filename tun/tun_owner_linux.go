//go:build linux

package tun

import (
	"fmt"
	"os"
	"strings"

	"phaethon/util"
)

// tunOwnerFile records that this host's tun0 was created by phaethon. It is
// written immediately before CreateDevice and removed on a clean StopTUN, so
// a crash leaves the marker behind and CleanupResidual can remove the stale
// interface precisely (name + type) without deleting another app's tun0.
const tunOwnerFile = "/var/run/phaethon-tun.owner"

const tunOwnerMagic = "PHAETHON-TUN-OWNER-v1"

func recordTUNOwner(iface string) {
	data := fmt.Sprintf("%s\niface=%s\npid=%d\n", tunOwnerMagic, iface, os.Getpid())
	if err := os.WriteFile(tunOwnerFile, []byte(data), 0644); err != nil {
		util.LogModuleWarn("cleanup", "write tun owner marker fail: %v", err)
	}
}

func clearTUNOwner() {
	_ = os.Remove(tunOwnerFile)
}

// readTUNOwner returns the recorded interface name when the marker belongs to
// this program. Missing/garbled markers are treated as "not ours".
func readTUNOwner() (string, bool) {
	data, err := os.ReadFile(tunOwnerFile)
	if err != nil {
		return "", false
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != tunOwnerMagic {
		return "", false
	}
	for _, line := range lines[1:] {
		if strings.HasPrefix(line, "iface=") {
			name := strings.TrimSpace(strings.TrimPrefix(line, "iface="))
			if name != "" {
				return name, true
			}
		}
	}
	// Magic matched but no iface: still ours; default to tun0.
	return "tun0", true
}
