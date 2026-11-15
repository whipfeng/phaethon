//go:build !linux && !darwin

package frame

import "net"

// setNotSentLowat is the Windows fallback: TCP_NOTSENT_LOWAT is Linux-only,
// so bound the socket send buffer instead. The initial default is small
// (16KB); the stripe controller may grow it toward 2×BDP for fast links.
func setNotSentLowat(tc *net.TCPConn, n int) error {
	return tc.SetWriteBuffer(n)
}

func defaultLowat() int { return 16 << 10 }
