//go:build windows

package tun

import (
	"syscall"

	"phaethon/util"
)

// bindToInterface returns a Control function that binds IPv4 sockets to the
// given interface index using IP_UNICAST_IF (IPPROTO_IP option 31). This works
// on all Windows adapter types (physical, VMware, Wintun) unlike syscall.Bind
// which fails in Control callbacks on Windows.
func bindToInterface(ifIndex int) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		var sockErr error
		err := c.Control(func(fd uintptr) {
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

// watchdogControl returns the interface-index binding function used by the
// watchdog probe dialer. On Windows IP_UNICAST_IF works on all adapter types
// including Wintun.
func watchdogControl(ifIndex int) func(network, address string, c syscall.RawConn) error {
	return bindToInterface(ifIndex)
}

// htonl converts a uint32 from host to network byte order. IP_UNICAST_IF
// requires the interface index in network byte order.
func htonl(val uint32) uint32 {
	return (val&0xFF)<<24 | (val&0xFF00)<<8 | (val&0xFF0000)>>8 | (val&0xFF000000)>>24
}
