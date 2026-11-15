//go:build !linux && !darwin

package frame

import "net"

// setNotSentLowat is the Windows fallback: TCP_NOTSENT_LOWAT is Linux-only,
// so bound the socket send buffer instead. Keep it small (64KB): the VM is the
// return path for interactive traffic and a 512KB SO_SNDBUF reintroduced
// second-level head-of-line delay.
func setNotSentLowat(tc *net.TCPConn, n int) error {
	const maxSendBuffer = 64 << 10
	if n > maxSendBuffer {
		n = maxSendBuffer
	}
	return tc.SetWriteBuffer(n)
}
