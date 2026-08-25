package util

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
)

// ReadByte reads a single byte from r.
func ReadByte(r io.Reader) (byte, error) {
	b := make([]byte, 1)
	_, err := io.ReadFull(r, b)
	return b[0], err
}

// ReadPort reads a 2-byte big-endian port from r.
func ReadPort(r io.Reader) (int, error) {
	buf := make([]byte, 2)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, err
	}
	return int(binary.BigEndian.Uint16(buf)), nil
}

// ReadLength reads a 2-byte big-endian length from r.
func ReadLength(r io.Reader) (int, error) {
	buf := make([]byte, 2)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, err
	}
	return int(binary.BigEndian.Uint16(buf)), nil
}

// EncodeTrojanAddr encodes host:port into Trojan ATYP + addr bytes.
func EncodeTrojanAddr(host string, port int) (atyp byte, addrBytes []byte, err error) {
	ip := net.ParseIP(host)
	if ip4 := ip.To4(); ip4 != nil {
		atyp = 0x01
		addrBytes = append(addrBytes, ip4...)
	} else if ip != nil {
		atyp = 0x04
		addrBytes = append(addrBytes, ip.To16()...)
	} else {
		if len(host) > 255 {
			return 0, nil, fmt.Errorf("trojan: domain name too long: %d bytes", len(host))
		}
		atyp = 0x03
		addrBytes = append(addrBytes, byte(len(host)))
		addrBytes = append(addrBytes, []byte(host)...)
	}
	portBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(portBuf, uint16(port))
	addrBytes = append(addrBytes, portBuf...)
	return atyp, addrBytes, nil
}

// EncodeTrojanAddrBytes encodes IP:port into Trojan ATYP + addr bytes.
func EncodeTrojanAddrBytes(ip net.IP, port int) (atyp byte, addrBytes []byte) {
	if ip4 := ip.To4(); ip4 != nil {
		atyp = 0x01
		addrBytes = append(addrBytes, ip4...)
	} else {
		atyp = 0x04
		addrBytes = append(addrBytes, ip.To16()...)
	}
	portBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(portBuf, uint16(port))
	addrBytes = append(addrBytes, portBuf...)
	return atyp, addrBytes
}

// EncodeTrojanAddrFromNetAddr encodes a net.Addr into Trojan ATYP + addr bytes.
func EncodeTrojanAddrFromNetAddr(addr net.Addr) (atyp byte, addrBytes []byte) {
	switch a := addr.(type) {
	case *net.UDPAddr:
		return EncodeTrojanAddrBytes(a.IP, a.Port)
	case *net.TCPAddr:
		return EncodeTrojanAddrBytes(a.IP, a.Port)
	}
	s := addr.String()
	host, portStr, _ := net.SplitHostPort(s)
	port, _ := strconv.Atoi(portStr)
	atyp, addrBytes, _ = EncodeTrojanAddr(host, port)
	return atyp, addrBytes
}
