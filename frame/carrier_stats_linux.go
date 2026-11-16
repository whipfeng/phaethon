//go:build linux

package frame

import (
	"net"

	"golang.org/x/sys/unix"
)

func carrierRetransStats(conn net.Conn) (uint64, uint64, bool) {
	c := conn
	for i := 0; i < 8; i++ {
		if tc, ok := c.(*net.TCPConn); ok {
			rc, err := tc.SyscallConn()
			if err != nil {
				return 0, 0, false
			}
			var info *unix.TCPInfo
			var serr error
			if err := rc.Control(func(fd uintptr) {
				info, serr = unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
			}); err != nil {
				return 0, 0, false
			}
			if serr != nil || info == nil {
				return 0, 0, false
			}
			return uint64(info.Segs_out), uint64(info.Total_retrans), true
		}
		u, ok := c.(interface{ NetConn() net.Conn })
		if !ok {
			break
		}
		c = u.NetConn()
	}
	return 0, 0, false
}
