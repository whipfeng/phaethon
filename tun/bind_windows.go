//go:build windows

package tun

import (
	"encoding/binary"
	"syscall"

	"golang.org/x/sys/windows"
)

// IP_UNICAST_IF is not defined in older versions of golang.org/x/sys/windows.
const ipUnicastIf = 31

// bindToInterface returns a net.Dialer Control function that binds IPv4 sockets
// to the given interface index using IP_UNICAST_IF. This forces outbound traffic
// to egress through the TUN adapter.
func bindToInterface(ifIndex int) func(network, address string, c syscall.RawConn) error {
	// IP_UNICAST_IF expects the interface index in network byte order.
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], uint32(ifIndex))
	idx := int(binary.LittleEndian.Uint32(buf[:]))

	return func(network, address string, c syscall.RawConn) error {
		var errBind error
		fn := func(fd uintptr) {
			errBind = windows.SetsockoptInt(windows.Handle(fd), windows.IPPROTO_IP, ipUnicastIf, idx)
		}
		if err := c.Control(fn); err != nil {
			return err
		}
		return errBind
	}
}

// watchdogControl returns a control function that binds IPv4 sockets to the
// given interface index using IP_UNICAST_IF. This forces watchdog probe traffic
// to egress through the TUN adapter.
func watchdogControl(ifIndex int) func(network, address string, c syscall.RawConn) error {
	return bindToInterface(ifIndex)
}
