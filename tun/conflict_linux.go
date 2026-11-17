//go:build linux

package tun

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/vishvananda/netlink"
)

// describeTUNConflict returns a one-line, read-only description of why tun0
// may be unavailable: the existing link's type/flags/addresses and the
// processes currently holding /dev/net/tun. It never deletes or modifies
// anything; the operator decides how to resolve the conflict.
func describeTUNConflict() string {
	var parts []string
	if link, err := netlink.LinkByName("tun0"); err == nil {
		addrs := make([]string, 0, 2)
		if v4, err := netlink.AddrList(link, netlink.FAMILY_V4); err == nil {
			for _, a := range v4 {
				addrs = append(addrs, a.IP.String())
			}
		}
		desc := fmt.Sprintf("tun0 exists type=%s flags=%s ipv4=[%s]",
			link.Type(), link.Attrs().Flags.String(), strings.Join(addrs, ","))
		if alias := link.Attrs().Alias; alias != "" {
			desc += " alias=" + alias
		}
		parts = append(parts, desc)
	}
	if pids := tunFDHolders(); len(pids) > 0 {
		parts = append(parts, "dev/net/tun holders: "+strings.Join(pids, ","))
	}
	if len(parts) == 0 {
		return "no tun0 link or /dev/net/tun holder found"
	}
	return strings.Join(parts, "; ")
}

// tunFDHolders lists processes with /dev/net/tun open as "pid(comm)".
func tunFDHolders() []string {
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []string
	for _, ent := range procs {
		pid, err := strconv.Atoi(ent.Name())
		if err != nil {
			continue
		}
		fdDir := filepath.Join("/proc", ent.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		held := false
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err == nil && target == "/dev/net/tun" {
				held = true
				break
			}
		}
		if !held {
			continue
		}
		comm, _ := os.ReadFile(filepath.Join("/proc", ent.Name(), "comm"))
		name := strings.TrimSpace(string(comm))
		if name == "" {
			name = "?"
		}
		out = append(out, fmt.Sprintf("%d(%s)", pid, name))
	}
	return out
}
