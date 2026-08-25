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
	socksAddr := "127.0.0.1:39980"
	targetHost := "8.8.8.8"
	targetPort := 53

	if len(os.Args) >= 2 {
		socksAddr = os.Args[1]
	}

	fmt.Printf("Test %d: SOCKS5=%s target=%s:%d\n", time.Now().Unix(), socksAddr, targetHost, targetPort)

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

	fmt.Printf("Sending %d bytes...\n", len(pkt))
	n, err := udpConn.WriteTo(pkt, relayAddr)
	if err != nil {
		fmt.Printf("FAIL: write udp packet: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Sent %d bytes\n", n)

	// 6. Wait for reply
	fmt.Println("Waiting for reply (10s timeout)...")
	udpConn.SetReadDeadline(time.Now().Add(10 * time.Second))
	recvBuf := make([]byte, 65536)
	n, addr, err := udpConn.ReadFrom(recvBuf)
	if err != nil {
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			fmt.Println("TIMEOUT: no reply received")
			os.Exit(1)
		}
		fmt.Printf("FAIL: read reply: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("SUCCESS: Got %d bytes from %s\n", n, addr)

	// Parse DNS response
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
			if len(dnsResp) >= 12 {
				ancount := int(binary.BigEndian.Uint16(dnsResp[6:8]))
				fmt.Printf("DNS: %d answers\n", ancount)
			}
		}
	}
	fmt.Println("PASS")
}

func buildDNSQuery(domain string) []byte {
	q := []byte{0x12, 0x34, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	for _, part := range splitDomain(domain) {
		q = append(q, byte(len(part)))
		q = append(q, []byte(part)...)
	}
	q = append(q, 0x00)
	q = append(q, 0x00, 0x01)
	q = append(q, 0x00, 0x01)
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
