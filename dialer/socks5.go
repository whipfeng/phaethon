package dialer

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"

	"phaethon/config"
	"phaethon/reverse"
	"phaethon/util"
)

// Socks5Dialer connects through a SOCKS5 proxy chain
type Socks5Dialer struct {
	BaseDialer
}

func (d *Socks5Dialer) DialPacket() (net.PacketConn, error) {
	// If this proxy is a reverse channel, use the reverse UDP tunnel
	if d.Proxy.ReverseAddress != "" {
		return (&ReverseDialer{BaseDialer: BaseDialer{Proxy: d.Proxy}}).DialPacket()
	}
	return Socks5UDPAssociate(d.Proxy)
}

func (d *Socks5Dialer) Dial(dstAddr string, dstPort int) (net.Conn, error) {
	// If this proxy is configured as a reverse channel, obtain from registry
	// and perform SOCKS5 CONNECT handshake so the remote Socks5Server can
	// resolve the real destination and relay traffic.
	if conn, err := d.TryReverse(); err != nil {
		return nil, fmt.Errorf("socks5: %w", err)
	} else if conn != nil {
		util.LogDebug("[SOCKS5-CLI] [%s] [%s] reverse match ok, doing CONNECT handshake to %s:%d", d.Proxy.Name, d.ConnIDStr(), dstAddr, dstPort)
		if err := socks5Handshake(conn, d.Proxy, dstAddr, dstPort, 0x01, d.ConnIDStr()); err != nil {
			conn.Close()
			return nil, fmt.Errorf("socks5: reverse handshake fail: %w", err)
		}
		util.LogDebug("[SOCKS5-CLI] [%s] [%s] reverse CONNECT handshake ok for %s:%d", d.Proxy.Name, d.ConnIDStr(), dstAddr, dstPort)
		return conn, nil
	}

	// First, connect to the next hop in the chain
	nextDialer := NewDialer(d.Proxy.Next)
	nextType := "nil"
	if d.Proxy.Next != nil {
		nextType = d.Proxy.Next.Type
	}
	util.LogDebug("[SOCKS5-DIAL] [%s] next=%s, dialing %s:%d", d.Proxy.Name, nextType, d.Proxy.Server, d.Proxy.Port)
	conn, err := nextDialer.Dial(d.Proxy.Server, d.Proxy.Port)
	if err != nil {
		return nil, fmt.Errorf("socks5: connect to server %s:%d fail: %w", d.Proxy.Server, d.Proxy.Port, err)
	}

	cmd := d.ResolveCmd(dstPort)

	// SOCKS5 handshake
	if err := socks5Handshake(conn, d.Proxy, dstAddr, dstPort, cmd, d.ConnIDStr()); err != nil {
		conn.Close()
		return nil, err
	}

	return conn, nil
}

// DialControl establishes a control connection to the registry through this SOCKS5 proxy.
// The proxy server IS the registry: it connects to proxy.Server:proxy.Port via the next hop,
// then performs a SOCKS5 BIND with PORT=1 to mark it as a control channel.
func (d *Socks5Dialer) DialControl() (net.Conn, error) {
	nextDialer := NewDialer(d.Proxy.Next)
	conn, err := nextDialer.Dial(d.Proxy.Server, d.Proxy.Port)
	if err != nil {
		return nil, fmt.Errorf("socks5: control connect to %s:%d fail: %w", d.Proxy.Server, d.Proxy.Port, err)
	}
	if err := socks5Handshake(conn, d.Proxy, d.Proxy.Server, reverse.BindPortControl, 0x02, d.ConnIDStr()); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func socks5Auth(conn net.Conn, proxy *config.Proxy) error {
	needAuth := proxy.Username != ""

	// Initial request
	var authMethod byte = 0x00 // NO_AUTH
	if needAuth {
		authMethod = 0x02 // PASSWORD
	}
	_, err := conn.Write([]byte{0x05, 0x01, authMethod})
	if err != nil {
		return fmt.Errorf("socks5: write initial request fail: %w", err)
	}

	// Initial response
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return fmt.Errorf("socks5: read initial response fail: %w", err)
	}
	if resp[0] != 0x05 {
		return fmt.Errorf("socks5: bad version: %d", resp[0])
	}

	// Password auth if needed
	if resp[1] == 0x02 && needAuth {
		authReq := []byte{0x01}
		authReq = append(authReq, byte(len(proxy.Username)))
		authReq = append(authReq, []byte(proxy.Username)...)
		authReq = append(authReq, byte(len(proxy.Password)))
		authReq = append(authReq, []byte(proxy.Password)...)
		if _, err := conn.Write(authReq); err != nil {
			return fmt.Errorf("socks5: write auth fail: %w", err)
		}
		authResp := make([]byte, 2)
		if _, err := io.ReadFull(conn, authResp); err != nil {
			return fmt.Errorf("socks5: read auth response fail: %w", err)
		}
		if authResp[1] != 0x00 {
			return fmt.Errorf("socks5: auth failed")
		}
	} else if resp[1] != 0x00 {
		return fmt.Errorf("socks5: unsupported auth method: %d", resp[1])
	}

	return nil
}

func socks5Handshake(conn net.Conn, proxy *config.Proxy, dstAddr string, dstPort int, cmd byte, connID string) error {
	if err := socks5Auth(conn, proxy); err != nil {
		return err
	}

	// Command request
	if len(dstAddr) > 255 {
		return fmt.Errorf("socks5: domain name too long: %d bytes", len(dstAddr))
	}
	cmdReq := []byte{0x05, cmd, 0x00, 0x03} // SOCKS5, CMD, RSV, DOMAIN
	cmdReq = append(cmdReq, byte(len(dstAddr)))
	cmdReq = append(cmdReq, []byte(dstAddr)...)
	portBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(portBuf, uint16(dstPort))
	cmdReq = append(cmdReq, portBuf...)
	if _, err := conn.Write(cmdReq); err != nil {
		return fmt.Errorf("socks5: write command fail: %w", err)
	}

	// Command response
	cmdResp := make([]byte, 4)
	if _, err := io.ReadFull(conn, cmdResp); err != nil {
		return fmt.Errorf("socks5: read command response fail: %w", err)
	}
	if cmdResp[1] != 0x00 {
		return fmt.Errorf("socks5: command failed, status: %d", cmdResp[1])
	}

	// Skip bound address
	switch cmdResp[3] {
	case 0x01: // IPv4
		skip := make([]byte, 4+2)
		if _, err := io.ReadFull(conn, skip); err != nil {
			return fmt.Errorf("socks5: read bind addr (IPv4) fail: %w", err)
		}
	case 0x03: // Domain
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(conn, lenBuf); err != nil {
			return fmt.Errorf("socks5: read bind addr len fail: %w", err)
		}
		skip := make([]byte, int(lenBuf[0])+2)
		if _, err := io.ReadFull(conn, skip); err != nil {
			return fmt.Errorf("socks5: read bind addr (domain) fail: %w", err)
		}
	case 0x04: // IPv6
		skip := make([]byte, 16+2)
		if _, err := io.ReadFull(conn, skip); err != nil {
			return fmt.Errorf("socks5: read bind addr (IPv6) fail: %w", err)
		}
	default:
		return fmt.Errorf("socks5: unknown bind address type: %d", cmdResp[3])
	}

	if connID == "" {
		connID = "N/A"
	}
	util.LogDebug("[SOCKS5-CLI] [%s] [%s] Connecting %s:%d via %s:%d", proxy.Name, connID, dstAddr, dstPort, proxy.Server, proxy.Port)
	return nil
}
