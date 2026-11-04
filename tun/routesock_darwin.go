//go:build darwin

package tun

import (
	"fmt"
	"net"
	"syscall"
	"unsafe"
)

// PF_ROUTE socket wrapper — the structured routing API on darwin (equivalent
// of netlink on linux). Replaces the route(8)/netstat(1) shell parsing in
// route_darwin.go with RTM_ADD/RTM_DELETE/RTM_GET messages that carry the
// interface index and typed sockaddrs.

const (
	rtmVersion     = 5
	rtmAdd         = 0x1
	rtmDelete      = 0x2
	rtmGet         = 0x4
	rtaDST         = 0x1
	rtaGATEWAY     = 0x2
	rtaNETMASK     = 0x4
	rtaIFP         = 0x10
	rtfUP          = 0x1
	rtfGATEWAY     = 0x2
	rtfSTATIC      = 0x800
	rtfPRCLONING   = 0x10000
	rtfIFSCOPE     = 0x1000000
	afINET         = 2
	afLINK         = 18
	iftOTHER       = 0x1 // utun registers IFT_OTHER (seen in RTM_GET replies)
	sockaddrInLen  = 16
	sockaddrDLLen  = 20
	rtmsgHdrLen   = int(unsafe.Sizeof(rtMsgHdr{}))
	sockaddrAlign  = 4
)

// Compile-time proof that the mirror matches xnu's layout (verified against
// the macOS 12.7 x86_64 SDK: sizeof(struct rt_msghdr) == 92).
var _ [unsafe.Sizeof(rtMsgHdr{})]byte = [92]byte{}

// rtMetrics mirrors struct rt_metrics. Despite darwin being LP64, every
// rt_metrics field is a 32-bit quantity in the SDK headers (verified with
// offsetof on macOS 12 x86_64: sizeof(struct rt_msghdr) == 92, rtm_rmx @ 36,
// sizeof(struct rt_metrics) == 56). Do not widen these to uint64.
type rtMetrics struct {
	locks, mtu, hopcount       uint32
	expire                     int32
	recvpipe, sendpipe         uint32
	ssthresh, rtt, rttvar      uint32
	pksent                     uint32
	filler                     [4]uint32
}

// rtMsgHdr mirrors struct rt_msghdr.
type rtMsgHdr struct {
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
	rmx     rtMetrics
}

type routeSock struct {
	fd  int
	seq int32
}

func newRouteSock() (*routeSock, error) {
	fd, err := syscall.Socket(syscall.AF_ROUTE, syscall.SOCK_RAW, 0)
	if err != nil {
		return nil, err
	}
	return &routeSock{fd: fd}, nil
}

func (r *routeSock) close() { syscall.Close(r.fd) }

// execute writes a route message and waits for the kernel's reply carrying
// our pid/seq, returning the reply header. rtm_errno surfaces as an error.
func (r *routeSock) execute(msg []byte) (*rtMsgHdr, []byte, error) {
	r.seq++
	hdr := (*rtMsgHdr)(unsafe.Pointer(&msg[0]))
	hdr.pid = int32(syscall.Getpid())
	hdr.seq = r.seq
	if _, err := syscall.Write(r.fd, msg); err != nil {
		return nil, nil, err
	}
	buf := make([]byte, 4096)
	for {
		n, err := syscall.Read(r.fd, buf)
		if err != nil {
			return nil, nil, err
		}
		if n < rtmsgHdrLen {
			continue
		}
		rh := (*rtMsgHdr)(unsafe.Pointer(&buf[0]))
		if rh.pid != int32(syscall.Getpid()) || rh.seq != r.seq {
			continue
		}
		if rh.errno != 0 {
			return rh, buf[:n], syscall.Errno(rh.errno)
		}
		return rh, buf[:n], nil
	}
}

// buildRouteMsg assembles an RTM message with the sockaddr list in bit order
// DST, GATEWAY, NETMASK. gwIP/gwIfIndex select the gateway form (inet vs
// link); exactly one of them is used when RTA_GATEWAY is set.
func buildRouteMsg(typ uint8, index int, flags int32, dst *net.IPNet, gwIP net.IP, gwIfIndex int, addrs int32) []byte {
	iface, ifaceErr := net.InterfaceByIndex(gwIfIndex)
	var ifType byte = iftOTHER
	var ifName string
	if ifaceErr == nil {
		ifName = iface.Name
	}
	var buf [1024]byte
	hdr := (*rtMsgHdr)(unsafe.Pointer(&buf[0]))
	hdr.version = rtmVersion
	hdr.typ = typ
	hdr.index = uint16(index)
	hdr.flags = flags
	hdr.addrs = addrs
	off := rtmsgHdrLen
	put := func(b []byte) {
		copy(buf[off:], b)
		off += len(b)
	}
	if addrs&rtaDST != 0 {
		put(sockaddrIn(dst.IP))
	}
	if addrs&rtaGATEWAY != 0 {
		if gwIP != nil {
			put(sockaddrIn(gwIP))
		} else {
			put(sockaddrDL(gwIfIndex, ifType, ifName))
		}
	}
	if addrs&rtaNETMASK != 0 {
		put(sockaddrIn(net.IP(dst.Mask)))
	}
	hdr.msglen = uint16(off)
	return buf[:off]
}

// sockaddrIn packs an IPv4 sockaddr_in (16 bytes, port 0).
func sockaddrIn(ip net.IP) []byte {
	b := make([]byte, sockaddrInLen)
	b[0] = sockaddrInLen
	b[1] = afINET
	copy(b[4:8], ip.To4())
	return b
}

// sockaddrDL packs an AF_LINK sockaddr_dl naming the interface by index and
// name, matching the form route(8) produces for -interface routes (20-byte
// minimum, sdl_type filled — the kernel blackholes packets for routes whose
// link gateway carries no type/name).
func sockaddrDL(index int, ifType byte, name string) []byte {
	nlen := len(name)
	l := 8 + nlen
	if l < sockaddrDLLen {
		l = sockaddrDLLen
	}
	b := make([]byte, l)
	b[0] = byte(l)
	b[1] = afLINK
	b[2] = byte(index)
	b[3] = byte(index >> 8)
	b[4] = ifType
	b[5] = byte(nlen)
	copy(b[8:], name)
	return b
}

// parseSockaddrs splits a route message body into per-bit sockaddr slices.
// xnu aligns each sockaddr to 4 bytes (ROUNDUP).
func parseSockaddrs(addrs int32, body []byte) map[int][]byte {
	m := make(map[int][]byte)
	off := 0
	for bit := 1; bit <= rtaIFP && off+2 <= len(body); bit <<= 1 {
		if addrs&int32(bit) == 0 {
			continue
		}
		l := int(body[off])
		if l == 0 || off+l > len(body) {
			break
		}
		m[bit] = body[off : off+l]
		off += (l + sockaddrAlign - 1) &^ (sockaddrAlign - 1)
	}
	return m
}

func sockaddrInIP(b []byte) net.IP {
	if len(b) < 8 || b[1] != afINET {
		return nil
	}
	return net.IPv4(b[4], b[5], b[6], b[7])
}

func isNoSuchRoute(err error) bool {
	return err == syscall.Errno(3) // ESRCH: no matching route
}

// routeAddNetViaInterface adds dst scoped to interface ifIndex, the PF_ROUTE
// equivalent of `route add -net dst -interface <utunN>`. Mirrors route(8):
// RTF_UP|RTF_STATIC|RTF_PRCLONING (no RTF_GATEWAY) with a fully-populated
// sockaddr_dl gateway — that exact shape is what gets packets delivered.
func routeAddNetViaInterface(ifIndex int, dst *net.IPNet) error {
	rs, err := newRouteSock()
	if err != nil {
		return err
	}
	defer rs.close()
	msg := buildRouteMsg(rtmAdd, ifIndex, rtfUP|rtfSTATIC|rtfPRCLONING, dst, nil, ifIndex, rtaDST|rtaGATEWAY|rtaNETMASK)
	_, _, err = rs.execute(msg)
	return err
}

// routeDeleteNetViaInterface deletes the dst route scoped to interface;
// ESRCH (route gone or points elsewhere) is not an error.
func routeDeleteNetViaInterface(ifIndex int, dst *net.IPNet) error {
	rs, err := newRouteSock()
	if err != nil {
		return err
	}
	defer rs.close()
	msg := buildRouteMsg(rtmDelete, ifIndex, rtfUP|rtfSTATIC|rtfPRCLONING, dst, nil, ifIndex, rtaDST|rtaGATEWAY|rtaNETMASK)
	_, _, err = rs.execute(msg)
	if isNoSuchRoute(err) {
		return nil
	}
	return err
}

// routeAddNetViaGateway adds dst via gateway IP (exclusion route form).
func routeAddNetViaGateway(dst *net.IPNet, gw net.IP) error {
	rs, err := newRouteSock()
	if err != nil {
		return err
	}
	defer rs.close()
	msg := buildRouteMsg(rtmAdd, 0, rtfUP|rtfGATEWAY|rtfSTATIC, dst, gw, 0, rtaDST|rtaGATEWAY|rtaNETMASK)
	_, _, err = rs.execute(msg)
	return err
}

// routeDeleteNetViaGateway deletes the dst-via-gw route; ESRCH is not an error.
func routeDeleteNetViaGateway(dst *net.IPNet, gw net.IP) error {
	rs, err := newRouteSock()
	if err != nil {
		return err
	}
	defer rs.close()
	msg := buildRouteMsg(rtmDelete, 0, rtfUP|rtfGATEWAY|rtfSTATIC, dst, gw, 0, rtaDST|rtaGATEWAY|rtaNETMASK)
	_, _, err = rs.execute(msg)
	if isNoSuchRoute(err) {
		return nil
	}
	return err
}

// defaultRoute returns the IPv4 default route's gateway and interface name
// via RTM_GET — the structured replacement for parsing `netstat -rn`.
// xnu 12 fills only DST/GATEWAY/NETMASK in the reply (no RTA_IFP), so the
// interface comes from the reply's rtm_index field.
func defaultRoute() (net.IP, string, error) {
	rs, err := newRouteSock()
	if err != nil {
		return nil, "", err
	}
	defer rs.close()
	zero := &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}
	msg := buildRouteMsg(rtmGet, 0, rtfUP, zero, nil, 0, rtaDST)
	hdr, resp, err := rs.execute(msg)
	if err != nil {
		return nil, "", err
	}
	if len(resp) < int(hdr.msglen) {
		return nil, "", fmt.Errorf("short RTM_GET response: %d < msglen %d", len(resp), hdr.msglen)
	}
	body := resp[rtmsgHdrLen:hdr.msglen]
	sas := parseSockaddrs(hdr.addrs, body)
	var gw net.IP
	if b := sas[rtaGATEWAY]; b != nil {
		gw = sockaddrInIP(b)
	}
	if gw == nil {
		return nil, "", fmt.Errorf("default route reply missing gateway (addrs=0x%x)", hdr.addrs)
	}
	iface, err := net.InterfaceByIndex(int(hdr.index))
	if err != nil {
		return nil, "", fmt.Errorf("default route ifindex %d: %w", hdr.index, err)
	}
	return gw, iface.Name, nil
}

// routeGetIfIndex returns the interface index the kernel would use for dst
// under the current routing table (RTM_GET). Structured replacement for
// `route -n get <dst>` parsing; callers compare against the TUN index to
// decide whether to fall back to the physical interface.
func routeGetIfIndex(dst net.IP) (int, error) {
	rs, err := newRouteSock()
	if err != nil {
		return 0, err
	}
	defer rs.close()
	dstNet := &net.IPNet{IP: dst, Mask: net.CIDRMask(32, 32)}
	msg := buildRouteMsg(rtmGet, 0, rtfUP, dstNet, nil, 0, rtaDST)
	hdr, _, err := rs.execute(msg)
	if err != nil {
		return 0, err
	}
	if hdr.index == 0 {
		return 0, fmt.Errorf("RTM_GET reply for %s has no interface index", dst)
	}
	return int(hdr.index), nil
}

// routeAddIfscopeDefault adds `default ifscope <ifIndex> via gw` — the scoped
// default that lets IP_BOUND_IF sockets on the physical interface resolve
// routes while the TUN holds the unscoped 0/1+128/1 hijack. IFSCOPE routes
// are invisible to unscoped lookups, so user traffic is unaffected.
// RTA_NETMASK is mandatory: without it the kernel accepts the route (netstat
// shows UGScIg) but keys it outside the scoped tree, so scoped lookups and
// route(8) both report "not in table" — verified empirically on macOS 12.
// EEXIST means an identical scoped default is already present (e.g. leftover
// from a SIGKILLed run); it is treated as applied so teardown still removes it.
func routeAddIfscopeDefault(gw net.IP, ifIndex int) error {
	rs, err := newRouteSock()
	if err != nil {
		return err
	}
	defer rs.close()
	zero := &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}
	msg := buildRouteMsg(rtmAdd, ifIndex, rtfUP|rtfGATEWAY|rtfSTATIC|rtfIFSCOPE, zero, gw, 0, rtaDST|rtaGATEWAY|rtaNETMASK)
	_, _, err = rs.execute(msg)
	if err == syscall.Errno(17) { // EEXIST
		return nil
	}
	return err
}

// routeDeleteIfscopeDefault deletes the scoped physical default; ESRCH is
// not an error.
func routeDeleteIfscopeDefault(gw net.IP, ifIndex int) error {
	rs, err := newRouteSock()
	if err != nil {
		return err
	}
	defer rs.close()
	zero := &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}
	msg := buildRouteMsg(rtmDelete, ifIndex, rtfUP|rtfGATEWAY|rtfSTATIC|rtfIFSCOPE, zero, gw, 0, rtaDST|rtaGATEWAY|rtaNETMASK)
	_, _, err = rs.execute(msg)
	if isNoSuchRoute(err) {
		return nil
	}
	return err
}
