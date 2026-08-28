//go:build windows

package tun

import (
	"net"
	"syscall"

	"phaethon/util"
)

// bindToInterface returns a no-op Control function on Windows. Windows cannot
// use syscall.Bind inside Control callbacks (fails with "invalid argument").
// Callers should use watchdogLocalAddr with net.Dialer.LocalAddr instead.
func bindToInterface(ifIndex int) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		return nil
	}
}

// watchdogControl returns nil on Windows. The watchdog probe uses
// watchdogLocalAddr with net.Dialer.LocalAddr instead.
func watchdogControl(ifIndex int) func(network, address string, c syscall.RawConn) error {
	return nil
}

// watchdogLocalAddr returns the local address of the TUN adapter to bind to.
// On Windows, this is the correct way to force traffic through TUN: set the
// source IP in the dialer, and the routing stack deterministically selects the
// TUN interface that owns that IP.
func watchdogLocalAddr(ifIndex int) net.Addr {
	localIP := selectTUNLocalIP(ifIndex)
	if localIP == nil {
		util.LogWarn("tun/bind: no suitable IP on interface %d, binding skipped", ifIndex)
		return nil
	}
	return &net.TCPAddr{IP: localIP}
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
