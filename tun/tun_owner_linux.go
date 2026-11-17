//go:build linux

package tun

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vishvananda/netlink"
	"phaethon/util"
)

// tunOwnerFile records that this host's tun0 was created by phaethon. It is
// written immediately after CreateDevice and removed on a clean StopTUN, so a
// crash leaves the marker behind and CleanupResidual can remove the stale
// interface precisely (name + instance + type + alias/MAC) without deleting
// another application's tun0.
const tunOwnerFile = "/var/run/phaethon-tun.owner"

const tunOwnerMagic = "PHAETHON-TUN-OWNER-v2"

// absInstance normalizes a data dir for comparison across process restarts.
func absInstance(instance string) string {
	if instance == "" {
		return ""
	}
	if abs, err := filepath.Abs(instance); err == nil {
		return abs
	}
	return instance
}

func recordTUNOwner(iface, instance string) {
	instance = absInstance(instance)
	mac := ""
	alias := fmt.Sprintf("phaethon:%s:%d:%d", instance, os.Getpid(), time.Now().UnixNano())
	if link, err := netlink.LinkByName(iface); err == nil {
		mac = link.Attrs().HardwareAddr.String()
		// IFLA_IFALIAS is a per-interface metadata tag that survives while the
		// interface exists and is readable by netlink. TUN devices have no MAC,
		// so this is the strongest Linux ownership proof for name-reuse races.
		if err := netlink.LinkSetAlias(link, alias); err != nil {
			util.LogModuleWarn("cleanup", "set tun owner alias fail: %v", err)
			alias = ""
		}
	} else {
		alias = ""
	}
	data := fmt.Sprintf("%s\niface=%s\ninstance=%s\nmac=%s\nalias=%s\npid=%d\n",
		tunOwnerMagic, iface, instance, mac, alias, os.Getpid())
	if err := os.WriteFile(tunOwnerFile, []byte(data), 0644); err != nil {
		util.LogModuleWarn("cleanup", "write tun owner marker fail: %v", err)
	}
}

func clearTUNOwner() {
	_ = os.Remove(tunOwnerFile)
}

// readTUNOwner returns the recorded ownership info when the marker belongs to
// this program. Missing/garbled markers are treated as "not ours".
func readTUNOwner() (tunOwnerInfo, bool) {
	data, err := os.ReadFile(tunOwnerFile)
	if err != nil {
		return tunOwnerInfo{}, false
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != tunOwnerMagic {
		return tunOwnerInfo{}, false
	}
	var info tunOwnerInfo
	for _, line := range lines[1:] {
		switch {
		case strings.HasPrefix(line, "iface="):
			info.iface = strings.TrimSpace(strings.TrimPrefix(line, "iface="))
		case strings.HasPrefix(line, "instance="):
			info.instance = strings.TrimSpace(strings.TrimPrefix(line, "instance="))
		case strings.HasPrefix(line, "mac="):
			info.mac = strings.TrimSpace(strings.TrimPrefix(line, "mac="))
		case strings.HasPrefix(line, "alias="):
			info.alias = strings.TrimSpace(strings.TrimPrefix(line, "alias="))
		}
	}
	if info.iface == "" {
		info.iface = "tun0"
	}
	return info, true
}
