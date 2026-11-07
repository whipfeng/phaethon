//go:build linux || darwin

package frame

import (
	"net"
	"testing"

	"golang.org/x/sys/unix"
)

type wrappedConn struct{ net.Conn }

func (w wrappedConn) NetConn() net.Conn { return w.Conn }

func TestSetNotSentLowat(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			defer c.Close()
			buf := make([]byte, 1)
			c.Read(buf)
		}
	}()
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	// Wrapped like a TLS lane: the TCP socket is reached via NetConn().
	limitKernelBacklog(wrappedConn{raw})

	rc, _ := raw.(*net.TCPConn).SyscallConn()
	var got int
	var gerr error
	rc.Control(func(fd uintptr) {
		got, gerr = unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT)
	})
	if gerr != nil {
		t.Fatal(gerr)
	}
	if got != laneNotSentLowat {
		t.Fatalf("TCP_NOTSENT_LOWAT = %d, want %d", got, laneNotSentLowat)
	}
}
