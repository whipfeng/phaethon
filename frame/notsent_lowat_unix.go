//go:build linux || darwin

package frame

import (
	"net"

	"golang.org/x/sys/unix"
)

func setNotSentLowat(tc *net.TCPConn, n int) error {
	if n <= 0 {
		return nil
	}
	// Also bound the total kernel queue (unsent + unacked). TCP_NOTSENT_LOWAT
	// alone only limits bytes that were never handed to the network: during a
	// cold start the congestion window can still grow well past the shallow
	// path buffer and queue a second of data in flight. The kernel doubles
	// SO_SNDBUF, so pass n/2 to make the effective budget ≈ n. The tuner grows
	// n toward 2×BDP once it has a steady rate sample, so fast links are not
	// capped.
	sndbuf := n / 2
	if sndbuf < 4<<10 {
		sndbuf = 4 << 10
	}
	if err := tc.SetWriteBuffer(sndbuf); err != nil {
		return err
	}
	rc, err := tc.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := rc.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT, n)
	}); err != nil {
		return err
	}
	return serr
}

func defaultLowat() int { return laneNotSentLowat }
