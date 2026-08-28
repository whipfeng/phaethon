//go:build windows

package tun

import (
	"syscall"

	"phaethon/util"
)

// bindToInterface returns a Control function that uses IP_UNICAST_IF to bind
// the socket to the specified interface. IP_UNICAST_IF works on all Windows
// adapter types (physical, Wintun, VMware, etc.).
func bindToInterface(ifIndex int) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		var sockErr error
		err := c.Control(func(fd uintptr) {
			// IP_UNICAST_IF requires interface index in network byte order
			ifIdx := htonl(uint32(ifIndex))
			sockErr = syscall.SetsockoptInt(
				syscall.Handle(fd),
				syscall.IPPROTO_IP,
				31, // IP_UNICAST_IF
				int(ifIdx),
			)
		})
		if err != nil {
			return err
		}
		if sockErr != nil {
			util.LogWarn("tun/bind: IP_UNICAST_IF for iface %d failed: %v", ifIndex, sockErr)
		}
		return sockErr
	}
}

// watchdogControl returns a Control function that binds the watchdog probe
// socket to the TUN interface using IP_UNICAST_IF.
func watchdogControl(ifIndex int) func(network, address string, c syscall.RawConn) error {
	return bindToInterface(ifIndex)
}

// htonl converts a uint32 from host to network byte order.
func htonl(val uint32) uint32 {
	return (val&0xFF)<<24 | (val&0xFF00)<<8 | (val&0xFF0000)>>8 | (val&0xFF000000)>>24
}
