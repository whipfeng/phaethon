//go:build !linux && !darwin

package frame

import "net"

// setNotSentLowat is the Windows fallback: TCP_NOTSENT_LOWAT is Linux-only,
// so bound the socket send buffer instead. 64KB is the platform floor; the
// stripe controller grows it toward 2×BDP for fast links and never shrinks
// below it (a 16KB floor capped a 29ms path at ~4.4Mbps).
func setNotSentLowat(tc *net.TCPConn, n int) error {
	return tc.SetWriteBuffer(n)
}

func defaultLowat() int { return laneNotSentLowat }
