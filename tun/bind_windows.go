//go:build windows

package tun

import (
	"net"
	"syscall"

	"phaethon/util"
)

// bindToInterface returns a net.Dialer Control function that binds sockets to
// the local IP address of the given interface. This forces outbound traffic to
// use that interface's address as the source, which deterministically selects
// the interface for egress (works for all adapter types including Wintun).
func bindToInterface(ifIndex int) func(network, address string, c syscall.RawConn) error {
	localIP := selectTUNLocalIP(ifIndex)
	if localIP == nil {
		util.LogWarn("tun/bind: no suitable IP on interface %d, binding skipped", ifIndex)
		return func(network, address string, c syscall.RawConn) error {
			return nil
		}
	}

	return func(network, address string, c syscall.RawConn) error {
		var bindErr error
		fn := func(fd uintptr) {
			if ip4 := localIP.To4(); ip4 != nil {
				sa := &syscall.SockaddrInet4{}
				copy(sa.Addr[:], ip4)
				bindErr = syscall.Bind(syscall.Handle(fd), sa)
			} else if ip16 := localIP.To16(); ip16 != nil {
				sa := &syscall.SockaddrInet6{}
				copy(sa.Addr[:], ip16)
				bindErr = syscall.Bind(syscall.Handle(fd), sa)
			}
		}
		if err := c.Control(fn); err != nil {
			return err
		}
		return bindErr
	}
}

// watchdogControl returns a control function that binds sockets to the TUN
// adapter's local IP. This forces watchdog probe traffic to egress through TUN.
func watchdogControl(ifIndex int) func(network, address string, c syscall.RawConn) error {
	return bindToInterface(ifIndex)
}

// selectTUNLocalIP picks the first non-link-local, non-loopback IP from the
// interface. For TUN adapters there is typically exactly one IP.
func selectTUNLocalIP(ifIndex int) net.IP {
	iface, err := net.InterfaceByIndex(ifIndex)
	if err != nil {
		util.LogWarn("tun/bind: interface %d not found: %v", ifIndex, err)
		return nil
	}

	addrs, err := iface.Addrs()
	if err != nil {
		util.LogWarn("tun/bind: failed to get addresses for interface %d: %v", ifIndex, err)
		return nil
	}

	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipNet.IP
		if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsLoopback() {
			continue
		}
		return ip
	}

	return nil
}
