//go:build linux

package tun

import (
	"net"
	"syscall"
)

// bindToInterface returns a net.Dialer Control function that binds sockets to
// the given interface index using SO_BINDTODEVICE. This forces outbound traffic
// to egress through the TUN adapter.
func bindToInterface(ifIndex int) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		iface, err := net.InterfaceByIndex(ifIndex)
		if err != nil {
			return err
		}
		var errBind error
		fn := func(fd uintptr) {
			errBind = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, iface.Name)
		}
		if err := c.Control(fn); err != nil {
			return err
		}
		return errBind
	}
}

// watchdogControl returns the interface-index binding function used by the
// watchdog probe dialer. On Linux SO_BINDTODEVICE respects the route table, so
// it works with the TUN virtual gateway.
func watchdogControl(ifIndex int) func(network, address string, c syscall.RawConn) error {
	return bindToInterface(ifIndex)
}

// watchdogLocalAddr returns nil on Linux. Linux uses Control-based binding
// (SO_BINDTODEVICE), not LocalAddr.
func watchdogLocalAddr(ifIndex int) net.Addr {
	return nil
}
