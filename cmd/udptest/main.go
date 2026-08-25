package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"time"
)

func main() {
	// Connect to SOCKS5 proxy via TCP (for UDP ASSOCIATE command)
	conn, err := net.Dial("tcp", "127.0.0.1:19901")
	if err != nil {
		log.Fatal("connect:", err)
	}
	defer conn.Close()

	// SOCKS5 handshake
	// Client greeting
	conn.Write([]byte{0x05, 0x01, 0x00}) // VER, NMETHODS, NO AUTH
	buf := make([]byte, 2)
	io.ReadFull(conn, buf) // VER, METHOD
	fmt.Printf("Auth response: %x\n", buf)

	// UDP ASSOCIATE request
	// VER=0x05, CMD=0x03 (UDP ASSOCIATE), RSV=0x00, ATYP=0x01 (IPv4)
	req := []byte{0x05, 0x03, 0x00, 0x01}
	// Bind address: all zeros (let server decide)
	req = append(req, 0, 0, 0, 0) // IP 0.0.0.0
	req = append(req, 0, 0)       // Port 0
	conn.Write(req)

	// Read SOCKS5 response: VER, REP, RSV, ATYP, BND.ADDR, BND.PORT
	resp := make([]byte, 4)
	io.ReadFull(conn, resp)
	fmt.Printf("UDP ASSOCIATE response: %x\n", resp)

	if resp[1] != 0x00 {
		log.Fatalf("UDP ASSOCIATE failed: REP=%02x", resp[1])
	}

	var bndAddr string
	switch resp[3] {
	case 0x01: // IPv4
		ip := make([]byte, 4)
		io.ReadFull(conn, ip)
		bndAddr = net.IP(ip).String()
	case 0x03: // Domain
		lenBuf := make([]byte, 1)
		io.ReadFull(conn, lenBuf)
		domain := make([]byte, lenBuf[0])
		io.ReadFull(conn, domain)
		bndAddr = string(domain)
	}
	portBuf := make([]byte, 2)
	io.ReadFull(conn, portBuf)
	bndPort := binary.BigEndian.Uint16(portBuf)
	fmt.Printf("UDP relay bound to: %s:%d\n", bndAddr, bndPort)

	// Now send UDP through a direct UDP socket to the relay address
	udpConn, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.ParseIP(bndAddr), Port: int(bndPort)})
	if err != nil {
		log.Fatal("UDP dial:", err)
	}
	defer udpConn.Close()

	// DNS query for www.baidu.com
	dnsQuery := []byte{
		0x00, 0x01, // ID
		0x01, 0x00, // Flags: standard query
		0x00, 0x01, // Questions: 1
		0x00, 0x00, // Answer RRs
		0x00, 0x00, // Authority RRs
		0x00, 0x00, // Additional RRs
		// Query: www.baidu.com
		0x03, 'w', 'w', 'w',
		0x05, 'b', 'a', 'i', 'd', 'u',
		0x03, 'c', 'o', 'm',
		0x00,       // null terminator
		0x00, 0x01, // Type A
		0x00, 0x01, // Class IN
	}

	// Wrap in SOCKS5 UDP header: RSV(2)=0x0000, FRAG=0x00, ATYP=0x01, DST.ADDR, DST.PORT, DATA
	udpPkt := []byte{0x00, 0x00, 0x00}  // RSV(2) + FRAG
	udpPkt = append(udpPkt, 0x01)       // ATYP: IPv4
	udpPkt = append(udpPkt, 8, 8, 8, 8) // 8.8.8.8
	udpPkt = append(udpPkt, 0x00, 0x35) // Port 53
	udpPkt = append(udpPkt, dnsQuery...)

	n, err := udpConn.Write(udpPkt)
	if err != nil {
		log.Fatal("UDP write:", err)
	}
	fmt.Printf("UDP write OK: %d bytes\n", n)

	udpConn.SetReadDeadline(time.Now().Add(10 * time.Second))
	respBuf := make([]byte, 2048)
	n, err = udpConn.Read(respBuf)
	if err != nil {
		log.Fatal("UDP read:", err)
	}
	fmt.Printf("UDP read OK: %d bytes\n", n)
	// Skip SOCKS5 UDP header (RSV, FRAG, ATYP, DST.ADDR, DST.PORT)
	// ATYP=0x01: 3+4+2=9, ATYP=0x03: 3+var, ATYP=0x04: 3+16+2=21
	fmt.Printf("UDP payload: %x\n", respBuf[10:n])
}
