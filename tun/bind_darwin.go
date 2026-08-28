//go:build darwin

package tun

import (
	"net"
	"syscall"
)

// bindToInterface returns a net.Dialer Control function that binds IPv4 sockets
// to the given interface index using IP_BOUND_IF. This forces outbound traffic
// to egress through the TUN adapter.
func bindToInterface(ifIndex int) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		var errBind error
		fn := func(fd uintptr) {
			errBind = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_BOUND_IF, ifIndex)
		}
		if err := c.Control(fn); err != nil {
			return err
		}
		return errBind
	}
}

// watchdogControl returns the interface-index binding function used by the
// watchdog probe dialer. On Darwin IP_BOUND_IF respects the route table, so it
// works with the TUN virtual gateway.
func watchdogControl(ifIndex int) func(network, address string, c syscall.RawConn) error {
	return bindToInterface(ifIndex)
}

// watchdogLocalAddr returns nil on Darwin. Darwin uses Control-based binding
// (IP_BOUND_IF), not LocalAddr.
func watchdogLocalAddr(ifIndex int) net.Addr {
	return nil
}
