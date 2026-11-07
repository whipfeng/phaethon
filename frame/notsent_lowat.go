package frame

import (
	"net"

	"phaethon/util"
)

// laneNotSentLowat caps unsent bytes in the kernel send buffer of a lane
// socket. Without it a slow egress lets ~10MB pile up in the kernel, behind
// which pings/pongs wait long enough to declare a live link dead; the backlog
// stays in the app-level DRR queues instead.
const laneNotSentLowat = 128 << 10

// limitKernelBacklog sets TCP_NOTSENT_LOWAT on the TCP socket under conn,
// unwrapping NetConn() layers (e.g. *tls.Conn). Non-TCP carriers are skipped.
func limitKernelBacklog(conn net.Conn) {
	c := conn
	for i := 0; i < 8; i++ {
		if tc, ok := c.(*net.TCPConn); ok {
			if err := setNotSentLowat(tc, laneNotSentLowat); err != nil {
				util.LogModuleInfo("p2p", "[LANE-LOWAT] skipped remote=%s: %v", conn.RemoteAddr(), err)
				return
			}
			util.LogModuleInfo("p2p", "[LANE-LOWAT] set %dKB remote=%s", laneNotSentLowat>>10, conn.RemoteAddr())
			return
		}
		u, ok := c.(interface{ NetConn() net.Conn })
		if !ok {
			break
		}
		c = u.NetConn()
	}
	util.LogModuleInfo("p2p", "[LANE-LOWAT] skipped remote=%s: carrier %T is not a TCP socket", conn.RemoteAddr(), c)
}
