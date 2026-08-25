package util

import (
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
)

// BuildUDPAddrHeader encodes a net.Addr in SOCKS5 UDP address format:
// [ATYP(1)] [DST.ADDR] [DST.PORT(2)].
// ATYP: 0x01=IPv4, 0x03=Domain, 0x04=IPv6.
func BuildUDPAddrHeader(addr net.Addr) ([]byte, error) {
	switch a := addr.(type) {
	case *net.UDPAddr:
		return buildUDPAddr(a.IP, a.Port), nil
	case *net.TCPAddr:
		return buildUDPAddr(a.IP, a.Port), nil
	}

	// Try to parse as host:port string
	s := addr.String()
	host, portStr, err := net.SplitHostPort(s)
	if err != nil {
		return nil, fmt.Errorf("invalid address format: %s", s)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("invalid port: %s", portStr)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Domain
		if len(host) > 255 {
			return nil, fmt.Errorf("domain name too long: %d", len(host))
		}
		header := make([]byte, 0, 1+1+len(host)+2)
		header = append(header, 0x03, byte(len(host)))
		header = append(header, []byte(host)...)
		portBuf := make([]byte, 2)
		binary.BigEndian.PutUint16(portBuf, uint16(port))
		header = append(header, portBuf...)
		return header, nil
	}
	return buildUDPAddr(ip, port), nil
}

func buildUDPAddr(ip net.IP, port int) []byte {
	if ip4 := ip.To4(); ip4 != nil {
		header := make([]byte, 1+4+2)
		header[0] = 0x01
		copy(header[1:], ip4)
		binary.BigEndian.PutUint16(header[5:], uint16(port))
		return header
	}
	ip16 := ip.To16()
	header := make([]byte, 1+16+2)
	header[0] = 0x04
	copy(header[1:], ip16)
	binary.BigEndian.PutUint16(header[17:], uint16(port))
	return header
}

// ParseUDPAddrHeader parses a SOCKS5 UDP address header and returns the source
// address, remaining data, and any error.
func ParseUDPAddrHeader(b []byte) (net.Addr, []byte, error) {
	if len(b) < 1 {
		return nil, nil, fmt.Errorf("udpaddr: empty header")
	}
	atyp := b[0]
	switch atyp {
	case 0x01: // IPv4
		if len(b) < 1+4+2 {
			return nil, nil, fmt.Errorf("udpaddr: short IPv4 header")
		}
		ip := net.IP(b[1:5])
		port := int(binary.BigEndian.Uint16(b[5:7]))
		return &net.UDPAddr{IP: ip, Port: port}, b[7:], nil
	case 0x03: // Domain
		if len(b) < 2 {
			return nil, nil, fmt.Errorf("udpaddr: short domain header")
		}
		domainLen := int(b[1])
		if len(b) < 2+domainLen+2 {
			return nil, nil, fmt.Errorf("udpaddr: short domain data")
		}
		host := string(b[2 : 2+domainLen])
		port := int(binary.BigEndian.Uint16(b[2+domainLen : 2+domainLen+2]))
		addr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			return nil, nil, err
		}
		return addr, b[2+domainLen+2:], nil
	case 0x04: // IPv6
		if len(b) < 1+16+2 {
			return nil, nil, fmt.Errorf("udpaddr: short IPv6 header")
		}
		ip := net.IP(b[1:17])
		port := int(binary.BigEndian.Uint16(b[17:19]))
		return &net.UDPAddr{IP: ip, Port: port}, b[19:], nil
	default:
		return nil, nil, fmt.Errorf("udpaddr: unknown address type: %d", atyp)
	}
}
