//go:build !windows && !linux && !darwin

package tun

import (
	"errors"
	"net"
	"syscall"
)

// bindToInterface returns a net.Dialer Control function that reports an error on
// platforms where interface-index socket binding is not implemented.
func bindToInterface(ifIndex int) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		return errors.New("TUN probe interface binding not implemented on this platform")
	}
}

// watchdogControl returns nil on unsupported platforms. The probe will still
// bind its source address to the TUN host IP if an interface index is provided,
// but there is no OS-level socket binding available.
func watchdogControl(ifIndex int) func(network, address string, c syscall.RawConn) error {
	return nil
}

// watchdogLocalAddr returns nil on unsupported platforms.
func watchdogLocalAddr(ifIndex int) net.Addr {
	return nil
}
