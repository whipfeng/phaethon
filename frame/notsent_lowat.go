package frame

import (
	"net"
	"os"
	"strconv"

	"phaethon/util"
)

// laneNotSentLowat caps unsent bytes in the kernel send buffer of a P2P carrier
// socket. It exists so a slow egress cannot pile megabytes into the kernel,
// behind which pings/pongs would wait long enough to declare a live link dead;
// the backlog stays in the app-level DRR queues instead.
//
// Sizing matters: this cap is a hard throughput ceiling of lowat/RTT because
// the socket can never keep more than this in flight.
//
//	128KB (old) @ 37ms RTT  -> ~3.4MB/s   (matched the observed 1-hop ceiling)
//	128KB (old) @ 56ms RTT  -> ~2.3MB/s   (matched the observed 2-hop ceiling)
//	512KB       @ 37ms RTT  -> ~13.8MB/s
//	512KB       @ 56ms RTT  -> ~9.1MB/s
//
// 128KB was clearly below the BDP of our links and became the real bottleneck.
// 512KB keeps control frames bounded (~150ms at 3MB/s) while leaving headroom
// for the link itself. Override with PHAETHON_P2P_LOWAT (bytes) when tuning.
const laneNotSentLowat = 512 << 10

func laneLowatBytes() int {
	if v := os.Getenv("PHAETHON_P2P_LOWAT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
		util.LogModuleWarn("p2p", "[LANE-LOWAT] invalid PHAETHON_P2P_LOWAT=%q, using %d", v, laneNotSentLowat)
	}
	return laneNotSentLowat
}

// limitKernelBacklog sets TCP_NOTSENT_LOWAT on the TCP socket under conn,
// unwrapping NetConn() layers (e.g. *tls.Conn). Non-TCP carriers are skipped.
// Returns whether the limit is in effect.
func limitKernelBacklog(conn net.Conn) bool {
	c := conn
	for i := 0; i < 8; i++ {
		if tc, ok := c.(*net.TCPConn); ok {
			lowat := laneLowatBytes()
			if err := setNotSentLowat(tc, lowat); err != nil {
				util.LogModuleInfo("p2p", "[LANE-LOWAT] skipped remote=%s: %v", conn.RemoteAddr(), err)
				return false
			}
			util.LogModuleInfo("p2p", "[LANE-LOWAT] set %dKB remote=%s", lowat>>10, conn.RemoteAddr())
			return true
		}
		u, ok := c.(interface{ NetConn() net.Conn })
		if !ok {
			break
		}
		c = u.NetConn()
	}
	util.LogModuleInfo("p2p", "[LANE-LOWAT] skipped remote=%s: carrier %T is not a TCP socket", conn.RemoteAddr(), c)
	return false
}

// KernelBacklogLimited is implemented by transports that know whether the
// carrier's unsent backlog is bounded (TCP_NOTSENT_LOWAT in effect).
type KernelBacklogLimited interface {
	KernelBacklogLimited() bool
}
