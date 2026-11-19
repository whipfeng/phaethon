package frame

import (
	"net"
	"os"
	"strconv"

	"phaethon/util"
)

// laneNotSentLowat is the initial cap before the stripe controller has any
// rate sample. 128KB is 2×BDP at a 29ms path (~4.4MB/s), enough to reach the
// measured shared-path ceiling; 64KB was exactly 2.2MB/s at 29ms and matched
// the observed mesh aggregate. On Windows this is SetWriteBuffer (there is no
// TCP_NOTSENT_LOWAT); the controller grows it toward 2×BDP (max 512KB) with
// hysteresis and never shrinks below this floor.
//
// Reference points measured on the mesh:
//
//	128KB (old) @ 37ms RTT  -> ~3.4MB/s   (matched the observed 1-hop ceiling)
//	128KB (old) @ 56ms RTT  -> ~2.3MB/s   (matched the observed 2-hop ceiling)
//	512KB       @ 37ms RTT  -> ~13.8MB/s
//	512KB       @ 56ms RTT  -> ~9.1MB/s
//
// PHAETHON_P2P_LOWAT is honored only with PHAETHON_LAB=1 for A/B tests;
// production uses the adaptive controller below.
const laneNotSentLowat = 128 << 10

func laneLowatBytes() int {
	if os.Getenv("PHAETHON_LAB") == "1" {
		if v := os.Getenv("PHAETHON_P2P_LOWAT"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				return n
			}
			util.LogModuleWarn("p2p", "[LANE-LOWAT] invalid PHAETHON_P2P_LOWAT=%q, using %d", v, defaultLowat())
		}
	}
	return defaultLowat()
}

// limitKernelBacklog sets TCP_NOTSENT_LOWAT on the TCP socket under conn,
// unwrapping NetConn() layers (e.g. *tls.Conn). Non-TCP carriers are skipped.
// Returns whether the limit is in effect.
func limitKernelBacklog(conn net.Conn) bool {
	c := conn
	for i := 0; i < 8; i++ {
		if tc, ok := c.(*net.TCPConn); ok {
			applyCarrierCC(tc, conn.RemoteAddr())
			// Optional per-socket pacing cap (bytes/s): bounds slow-start
			// bursts into a shallow WAN buffer. Inert unless the env is set.
			if v := os.Getenv("PHAETHON_CARRIER_PACING_BPS"); v != "" {
				if bps, perr := strconv.ParseUint(v, 10, 64); perr == nil && bps > 0 {
					if err := setMaxPacingRate(tc, bps); err != nil {
						util.LogModuleWarn("p2p", "[CARRIER-PACING] set %d B/s failed remote=%s: %v", bps, conn.RemoteAddr(), err)
					} else {
						util.LogModuleInfo("p2p", "[CARRIER-PACING] set %d B/s remote=%s", bps, conn.RemoteAddr())
					}
				}
			}
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

// applyCarrierCC selects a per-socket congestion-control algorithm when
// PHAETHON_CARRIER_CC is set (e.g. "bbr"). This is a lab/reference switch for
// the isolated BBR experiment: it is inert by default, changes no global
// sysctl, and only affects this process's carrier sockets. The shipping path is
// the portable app-level pacer.
func applyCarrierCC(tc *net.TCPConn, remote net.Addr) {
	cc := os.Getenv("PHAETHON_CARRIER_CC")
	if cc == "" {
		return
	}
	if err := setCongestionControl(tc, cc); err != nil {
		util.LogModuleWarn("p2p", "[CARRIER-CC] set %q failed remote=%s: %v", cc, remote, err)
		return
	}
	util.LogModuleInfo("p2p", "[CARRIER-CC] set %q remote=%s", cc, remote)
}

// KernelBacklogLimited is implemented by transports that know whether the
// carrier's unsent backlog is bounded (TCP_NOTSENT_LOWAT in effect).
type KernelBacklogLimited interface {
	KernelBacklogLimited() bool
}

// KernelBacklogTuner is implemented by transports that can adjust the kernel
// unsent backlog at runtime. The stripe controller uses it to keep the kernel
// queue near twice the measured BDP: small on slow links (bounded interactive
// latency), large on fast links (no throughput ceiling).
type KernelBacklogTuner interface {
	SetKernelBacklogLimit(n int) bool
}
