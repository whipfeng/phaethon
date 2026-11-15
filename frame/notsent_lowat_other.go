//go:build !linux && !darwin

package frame

import "net"

// setNotSentLowat is the Windows fallback: TCP_NOTSENT_LOWAT is Linux-only,
// so bound the socket send buffer instead. SO_SNDBUF limits queued data and
// therefore keeps the same "backpressure reaches the app" property, at the
// cost of including in-flight bytes.
func setNotSentLowat(tc *net.TCPConn, n int) error {
	if err := tc.SetWriteBuffer(n); err != nil {
		return err
	}
	return nil
}
