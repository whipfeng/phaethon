package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

func main() {
	targetHost := "8.8.8.8"
	targetPort := 53

	if len(os.Args) >= 3 {
		targetHost = os.Args[1]
		targetPort = 0
		fmt.Sscanf(os.Args[2], "%d", &targetPort)
	}

	socksAddr := "127.0.0.1:19901"
	fmt.Printf("Connecting SOCKS5 %s ...\n", socksAddr)

	// 1. TCP connect to SOCKS5 server
	tcpConn, err := net.DialTimeout("tcp", socksAddr, 5*time.Second)
	if err != nil {
		fmt.Printf("FAIL: dial socks5: %v\n", err)
		os.Exit(1)
	}
	defer tcpConn.Close()

	// 2. No-auth handshake
	if _, err := tcpConn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		fmt.Printf("FAIL: write auth: %v\n", err)
		os.Exit(1)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(tcpConn, resp); err != nil {
		fmt.Printf("FAIL: read auth resp: %v\n", err)
		os.Exit(1)
	}
	if resp[0] != 0x05 || resp[1] != 0x00 {
		fmt.Printf("FAIL: auth rejected: %x\n", resp)
		os.Exit(1)
	}
	fmt.Println("Auth OK")

	// 3. UDP ASSOCIATE request
	req := []byte{0x05, 0x03, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
	if _, err := tcpConn.Write(req); err != nil {
		fmt.Printf("FAIL: write udp associate: %v\n", err)
		os.Exit(1)
	}

	header := make([]byte, 4)
	if _, err := io.ReadFull(tcpConn, header); err != nil {
		fmt.Printf("FAIL: read udp assoc header: %v\n", err)
		os.Exit(1)
	}
	if header[1] != 0x00 {
		fmt.Printf("FAIL: udp associate failed, status=%d\n", header[1])
		os.Exit(1)
	}
	fmt.Printf("UDP ASSOCIATE OK, header=%x\n", header)

	// Read bind addr
	addrBuf := make([]byte, 6) // IPv4 (0x01) + 4 bytes + 2 bytes port
	if _, err := io.ReadFull(tcpConn, addrBuf); err != nil {
		fmt.Printf("FAIL: read bind addr: %v\n", err)
		os.Exit(1)
	}
	relayPort := int(binary.BigEndian.Uint16(addrBuf[4:6]))
	relayAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: relayPort}
	fmt.Printf("UDP relay: %s\n", relayAddr)

	// 4. Create local UDP socket
	udpConn, err := net.ListenUDP("udp", nil)
	if err != nil {
		fmt.Printf("FAIL: create udp socket: %v\n", err)
		os.Exit(1)
	}
	defer udpConn.Close()

	// 5. Build a DNS query for google.com
	dnsPayload := buildDNSQuery("google.com")
	fmt.Printf("DNS query: google.com (%d bytes)\n", len(dnsPayload))

	// Build SOCKS5 UDP packet: RSV(2) + FRAG(1) + ATYP(1) + DST.ADDR + DST.PORT + DATA
	ip := net.ParseIP(targetHost)
	var pkt []byte
	pkt = append(pkt, 0x00, 0x00, 0x00) // RSV + FRAG
	if ip4 := ip.To4(); ip4 != nil {
		pkt = append(pkt, 0x01) // IPv4
		pkt = append(pkt, ip4...)
	} else {
		pkt = append(pkt, 0x03) // Domain
		pkt = append(pkt, byte(len(targetHost)))
		pkt = append(pkt, []byte(targetHost)...)
	}
	portBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(portBuf, uint16(targetPort))
	pkt = append(pkt, portBuf...)
	pkt = append(pkt, dnsPayload...)

	fmt.Printf("Sending %d bytes to %s:%d via UDP relay %s\n", len(pkt), targetHost, targetPort, relayAddr)
	n, err := udpConn.WriteTo(pkt, relayAddr)
	if err != nil {
		fmt.Printf("FAIL: write udp packet: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Sent %d bytes\n", n)

	// 6. Wait for reply
	fmt.Println("Waiting for reply (5s timeout)...")
	udpConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	recvBuf := make([]byte, 65536)
	n, addr, err := udpConn.ReadFrom(recvBuf)
	if err != nil {
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			fmt.Println("Timeout: no reply received")
		} else {
			fmt.Printf("FAIL: read reply: %v\n", err)
			os.Exit(1)
		}
	} else {
		fmt.Printf("Got %d bytes from %s\n", n, addr)
		// Parse SOCKS5 UDP header -> data starts at offset
		if n >= 10 {
			atype := recvBuf[3]
			var dataStart int
			switch atype {
			case 0x01:
				dataStart = 10
			case 0x03:
				dataStart = 7 + int(recvBuf[4])
			case 0x04:
				dataStart = 22
			default:
				dataStart = 4
			}
			if dataStart < n {
				dnsResp := recvBuf[dataStart:n]
				fmt.Printf("  Raw DNS: %x\n", dnsResp[:min(len(dnsResp), 80)])
				// Parse DNS answer
				parseDNSResponse(dnsResp)
			}
		}
	}

	fmt.Println("Done.")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func buildDNSQuery(domain string) []byte {
	// DNS header: ID(2) + Flags(2) + QDCOUNT(2) + ANCOUNT(2) + NSCOUNT(2) + ARCOUNT(2)
	q := []byte{0x12, 0x34, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	// Question: QNAME + QTYPE(2) + QCLASS(2)
	for _, part := range splitDomain(domain) {
		q = append(q, byte(len(part)))
		q = append(q, []byte(part)...)
	}
	q = append(q, 0x00)       // null terminator
	q = append(q, 0x00, 0x01) // QTYPE A
	q = append(q, 0x00, 0x01) // QCLASS IN
	return q
}

func splitDomain(domain string) []string {
	var parts []string
	start := 0
	for i, c := range domain {
		if c == '.' {
			parts = append(parts, domain[start:i])
			start = i + 1
		}
	}
	parts = append(parts, domain[start:])
	return parts
}

func parseDNSResponse(data []byte) {
	if len(data) < 12 {
		fmt.Println("  DNS: too short")
		return
	}
	ancount := int(binary.BigEndian.Uint16(data[6:8]))
	fmt.Printf("  DNS: %d answers\n", ancount)
	offset := 12
	// skip question
	for offset < len(data) && data[offset] != 0 {
		offset++
	}
	if offset < len(data) {
		offset++ // null
	}
	offset += 4 // QTYPE + QCLASS

	for i := 0; i < ancount && offset+10 <= len(data); i++ {
		// Handle name compression (0xc0)
		if offset < len(data) && data[offset]&0xc0 == 0xc0 {
			offset += 2 // pointer
		} else {
			for offset < len(data) && data[offset] != 0 {
				offset++
			}
			offset++ // null
		}
		if offset+10 > len(data) {
			break
		}
		rtype := binary.BigEndian.Uint16(data[offset:])
		rclass := binary.BigEndian.Uint16(data[offset+2:])
		// ttl := binary.BigEndian.Uint32(data[offset+4:])
		rdlength := int(binary.BigEndian.Uint16(data[offset+8:]))
		rdata := data[offset+10 : offset+10+rdlength]
		offset += 10 + rdlength

		if rtype == 1 && rclass == 1 { // A record
			fmt.Printf("  A: %d.%d.%d.%d\n", rdata[0], rdata[1], rdata[2], rdata[3])
		}
	}
}
