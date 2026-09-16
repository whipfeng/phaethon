package dialer

import (
	"net"
	"strconv"

	"phaethon/util"
)

// DirectDialer connects directly to the destination via OS socket.
// Used by the DIRECT proxy rule for direct connections.
type DirectDialer struct{}

func (d *DirectDialer) Dial(dstAddr string, dstPort int) (net.Conn, error) {
	addr := net.JoinHostPort(dstAddr, strconv.Itoa(dstPort))
	conn, err := DialRouteAware("tcp", addr)
	if err != nil {
		return nil, err
	}
	util.SetTCPNoDelay(conn)
	return conn, nil
}

// ServerAddr returns empty values since DirectDialer has no proxy server.
func (d *DirectDialer) ServerAddr() (string, int) {
	return "", 0
}

func (d *DirectDialer) DialPacket() (net.PacketConn, error) {
	pc, err := ListenPacketRouteAware("udp", "")
	if err != nil {
		return nil, err
	}
	util.LogDebug("[DIRECT] UDP socket ready on %s", pc.LocalAddr())
	return pc, nil
}
