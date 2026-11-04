//go:build darwin

package dialer

import (
	"fmt"
	"net"
	"syscall"
	"unsafe"
)

// Minimal PF_ROUTE client for route lookups — replaces forking `route -n get`
// per destination. Only RTM_GET is implemented; the dialer never mutates
// routes. Layout mirrors xnu's struct rt_msghdr (verified: sizeof == 92 on
// macOS 12 x86_64, rtm_rmx @ 36). Duplicated from tun/routesock_darwin.go
// because tun imports dialer (a shared package would create a cycle).

const (
	dRtmVersion    = 5
	dRtmGet        = 0x4
	dRtaDST        = 0x1
	dRtaGATEWAY    = 0x2
	dAfINET        = 2
	dSockaddrInLen = 16
	dHdrLen        = int(unsafe.Sizeof(dRtMsgHdr{}))
	dAlign         = 4
)

var _ [unsafe.Sizeof(dRtMsgHdr{})]byte = [92]byte{}

type dRtMetrics struct {
	locks, mtu, hopcount  uint32
	expire                int32
	recvpipe, sendpipe    uint32
	ssthresh, rtt, rttvar uint32
	pksent                uint32
	filler                [4]uint32
}

type dRtMsgHdr struct {
	msglen  uint16
	version uint8
	typ     uint8
	index   uint16
	flags   int32
	addrs   int32
	pid     int32
	seq     int32
	errno   int32
	use     uint32
	inits   uint32
	rmx     dRtMetrics
}

func dSockaddrIn(ip net.IP) []byte {
	b := make([]byte, dSockaddrInLen)
	b[0] = dSockaddrInLen
	b[1] = dAfINET
	copy(b[4:8], ip.To4())
	return b
}

// routeGetIfIndex returns the interface index the kernel would route dst
// through right now. ESRCH surfaces as an error; callers fall back to the
// default physical interface.
func routeGetIfIndex(dst net.IP) (int, error) {
	fd, err := syscall.Socket(syscall.AF_ROUTE, syscall.SOCK_RAW, 0)
	if err != nil {
		return 0, err
	}
	defer syscall.Close(fd)

	var buf [512]byte
	hdr := (*dRtMsgHdr)(unsafe.Pointer(&buf[0]))
	hdr.version = dRtmVersion
	hdr.typ = dRtmGet
	hdr.flags = 0x1 // RTF_UP
	hdr.addrs = dRtaDST
	off := dHdrLen
	copy(buf[off:], dSockaddrIn(dst))
	off += dSockaddrInLen
	hdr.msglen = uint16(off)

	hdr.pid = int32(syscall.Getpid())
	hdr.seq = 1
	if _, err := syscall.Write(fd, buf[:off]); err != nil {
		return 0, err
	}
	rbuf := make([]byte, 4096)
	for {
		n, err := syscall.Read(fd, rbuf)
		if err != nil {
			return 0, err
		}
		if n < dHdrLen {
			continue
		}
		rh := (*dRtMsgHdr)(unsafe.Pointer(&rbuf[0]))
		if rh.pid != int32(syscall.Getpid()) || rh.seq != 1 {
			continue
		}
		if rh.errno != 0 {
			return 0, syscall.Errno(rh.errno)
		}
		if rh.index == 0 {
			return 0, fmt.Errorf("RTM_GET reply for %s has no interface index", dst)
		}
		return int(rh.index), nil
	}
}
