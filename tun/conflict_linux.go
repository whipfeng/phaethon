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

// describeTUNConflict returns a one-line, read-only description of why a TUN
// device may be unavailable: every tun*/TAP link (type/flags/addresses/alias)
// and the processes currently holding /dev/net/tun. Interface names are
// kernel-assigned (tunN), so this never assumes one particular name.
func describeTUNConflict() string {
	var parts []string
	if links, err := netlink.LinkList(); err == nil {
		for _, link := range links {
			name := link.Attrs().Name
			if !strings.HasPrefix(name, "tun") && link.Type() != "tap" {
				continue
			}
			addrs := make([]string, 0, 2)
			if v4, err := netlink.AddrList(link, netlink.FAMILY_V4); err == nil {
				for _, a := range v4 {
					addrs = append(addrs, a.IP.String())
				}
			}
			desc := fmt.Sprintf("%s type=%s flags=%s ipv4=[%s]",
				name, link.Type(), link.Attrs().Flags.String(), strings.Join(addrs, ","))
			if alias := link.Attrs().Alias; alias != "" {
				desc += " alias=" + alias
			}
			parts = append(parts, desc)
		}
	}
	if pids := tunFDHolders(); len(pids) > 0 {
		parts = append(parts, "dev/net/tun holders: "+strings.Join(pids, ","))
	}
	if len(parts) == 0 {
		return "no tun link or /dev/net/tun holder found"
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
