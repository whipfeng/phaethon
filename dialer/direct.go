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
	// Netstack path: only for domain names that get resolved to fakeIP.
	// Direct IP connections should use OS sockets (DialRouteAware).
	if GlobalNetstackDialFunc != nil && GlobalDNSResolverFunc != nil {
		if ip := net.ParseIP(dstAddr); ip == nil {
			// dstAddr is a domain name, resolve to fakeIP via netstack
			fakeIP, err := GlobalDNSResolverFunc(dstAddr)
			if err != nil {
				return nil, fmt.Errorf("netstack dns resolve %s: %w", dstAddr, err)
			}
			addr := net.JoinHostPort(fakeIP.String(), strconv.Itoa(dstPort))
			conn, err := GlobalNetstackDialFunc("tcp", addr)
			if err != nil {
				return nil, err
			}
			util.SetTCPNoDelay(conn)
			return conn, nil
		}
	}

	// Direct IP connection or netstack not available: use OS socket
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
