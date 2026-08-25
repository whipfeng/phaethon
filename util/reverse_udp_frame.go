package util

import (
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
)

// ATYP values for Reverse UDP frames.
const (
	ReverseATYPv4     = 0x01 // IPv4 address follows
	ReverseATYPDomain = 0x03 // Domain name follows
	ReverseATYPv6     = 0x04 // IPv6 address follows
	ReverseATYPHeart  = 0xFF // Heartbeat (no addr/port/payload)
)

// BuildReverseUDPFrame constructs a Reverse UDP data frame (plaintext, before AEAD encryption).
//
// Format: RSV(2B=0x0000) + ATYP(1) + ADDR(variable) + PORT(2 big-endian) + PAYLOAD
func BuildReverseUDPFrame(targetAddr net.Addr, payload []byte) []byte {
	var frame []byte
	switch a := targetAddr.(type) {
	case *net.UDPAddr:
		frame = buildReverseAddr(a.IP, a.Port)
	case *net.TCPAddr:
		frame = buildReverseAddr(a.IP, a.Port)
	default:
		s := targetAddr.String()
		host, portStr, err := net.SplitHostPort(s)
		if err != nil {
			return nil
		}
		port, _ := strconv.Atoi(portStr)
		ip := net.ParseIP(host)
		if ip != nil {
			frame = buildReverseAddr(ip, port)
		} else {
			// Domain
			if len(host) > 255 {
				return nil
			}
			frame = make([]byte, 2+1+1+len(host)+2)
			frame[0] = 0x00 // RSV high
			frame[1] = 0x00 // RSV low
			frame[2] = 0x03
			frame[3] = byte(len(host))
			copy(frame[4:], []byte(host))
			binary.BigEndian.PutUint16(frame[4+len(host):], uint16(port))
		}
	}
	frame = append(frame, payload...)
	return frame
}

// BuildReverseUDPHeartbeat constructs an encrypted heartbeat frame (plaintext before AEAD).
// Format: RSV(2B=0x0000) + ATYP(1B=0xFF)
func BuildReverseUDPHeartbeat() []byte {
	return []byte{0x00, 0x00, ReverseATYPHeart}
}

func buildReverseAddr(ip net.IP, port int) []byte {
	if ip4 := ip.To4(); ip4 != nil {
		frame := make([]byte, 2+1+4+2, 2+1+4+2+64) // pre-alloc for typical payload
		frame[0] = 0x00                            // RSV high
		frame[1] = 0x00                            // RSV low
		frame[2] = 0x01                            // IPv4
		copy(frame[3:], ip4)
		binary.BigEndian.PutUint16(frame[7:], uint16(port))
		return frame
	}
	ip16 := ip.To16()
	frame := make([]byte, 2+1+16+2, 2+1+16+2+64)
	frame[0] = 0x00 // RSV high
	frame[1] = 0x00 // RSV low
	frame[2] = 0x04 // IPv6
	copy(frame[3:], ip16)
	binary.BigEndian.PutUint16(frame[19:], uint16(port))
	return frame
}

// ParseReverseUDPFrame parses a Reverse UDP frame (plaintext, after AEAD decryption).
// Returns the target address, payload data, and any error.
// For heartbeat frames (ATYP=0xFF), returns (nil, nil, nil).
func ParseReverseUDPFrame(data []byte) (targetAddr net.Addr, payload []byte, err error) {
	if len(data) < 3 { // minimum: RSV(2) + ATYP(1)
		return nil, nil, fmt.Errorf("reverse_udp_frame: too short (%d bytes)", len(data))
	}
	if data[0] != 0x00 || data[1] != 0x00 {
		return nil, nil, fmt.Errorf("reverse_udp_frame: invalid RSV")
	}
	atyp := data[2]
	if atyp == ReverseATYPHeart {
		return nil, nil, nil // heartbeat frame
	}
	if len(data) < 5 { // data frame needs addr + port
		return nil, nil, fmt.Errorf("reverse_udp_frame: too short for data (%d bytes)", len(data))
	}
	switch atyp {
	case 0x01: // IPv4
		if len(data) < 2+1+4+2 {
			return nil, nil, fmt.Errorf("reverse_udp_frame: short IPv4 address")
		}
		ip := net.IP(data[3:7])
		port := int(binary.BigEndian.Uint16(data[7:9]))
		return &net.UDPAddr{IP: ip, Port: port}, data[9:], nil

	case 0x03: // Domain
		if len(data) < 2+1+1+2 {
			return nil, nil, fmt.Errorf("reverse_udp_frame: short domain header")
		}
		domainLen := int(data[3])
		if len(data) < 2+1+domainLen+2 {
			return nil, nil, fmt.Errorf("reverse_udp_frame: short domain data")
		}
		host := string(data[4 : 4+domainLen])
		port := int(binary.BigEndian.Uint16(data[4+domainLen : 4+domainLen+2]))
		addr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			return nil, nil, err
		}
		return addr, data[4+domainLen+2:], nil

	case 0x04: // IPv6
		if len(data) < 2+1+16+2 {
			return nil, nil, fmt.Errorf("reverse_udp_frame: short IPv6 address")
		}
		ip := net.IP(data[3:19])
		port := int(binary.BigEndian.Uint16(data[19:21]))
		return &net.UDPAddr{IP: ip, Port: port}, data[21:], nil

	default:
		return nil, nil, fmt.Errorf("reverse_udp_frame: unknown address type: %d", atyp)
	}
}
