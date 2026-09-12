package dialer

import (
	"fmt"
	"net"
	"strconv"

	"phaethon/util"
)

// DirectDialer connects directly to the destination
type DirectDialer struct{}

func (d *DirectDialer) Dial(dstAddr string, dstPort int) (net.Conn, error) {
	if GlobalNetstackDialFunc != nil {
		dialAddr := dstAddr
		if ip := net.ParseIP(dstAddr); ip == nil {
			if GlobalDNSResolverFunc != nil {
				fakeIP, err := GlobalDNSResolverFunc(dstAddr)
				if err != nil {
					return nil, fmt.Errorf("netstack dns resolve %s: %w", dstAddr, err)
				}
				dialAddr = fakeIP.String()
			}
		}
		addr := net.JoinHostPort(dialAddr, strconv.Itoa(dstPort))
		conn, err := GlobalNetstackDialFunc("tcp", addr)
		if err != nil {
			return nil, err
		}
		util.SetTCPNoDelay(conn)
		return conn, nil
	}

	addr := net.JoinHostPort(dstAddr, strconv.Itoa(dstPort))
	conn, err := DialRouteAware("tcp", addr)
	if err != nil {
		return nil, err
	}
	util.SetTCPNoDelay(conn)
	return conn, nil
}

func (d *DirectDialer) DialPacket() (net.PacketConn, error) {
	pc, err := ListenPacketRouteAware("udp", "")
	if err != nil {
		return nil, err
	}
	util.LogDebug("[DIRECT] UDP socket ready on %s", pc.LocalAddr())
	return pc, nil
}
