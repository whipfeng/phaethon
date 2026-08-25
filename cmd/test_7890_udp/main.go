package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"net"
	"time"
)

func main() {
	proxyAddr := flag.String("proxy", "127.0.0.1:7890", "SOCKS5 proxy address (host:port)")
	targetAddr := flag.String("target", "127.0.0.1:9999", "target echo server address (host:port)")
	flag.Parse()

	// 1. TCP connect to proxy
	conn, err := net.DialTimeout("tcp", *proxyAddr, 5*time.Second)
	if err != nil {
		fmt.Println("TCP connect fail:", err)
		return
	}
	defer conn.Close()
	fmt.Println("Connected to", *proxyAddr)

	// 2. SOCKS5 auth
	conn.Write([]byte{0x05, 0x01, 0x00})
	resp := make([]byte, 2)
	io.ReadFull(conn, resp)
	fmt.Printf("Auth: %v\n", resp)

	// 3. UDP ASSOCIATE (dst = 0.0.0.0:0)
	conn.Write([]byte{0x05, 0x03, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	resp2 := make([]byte, 262)
	n, _ := conn.Read(resp2)
	fmt.Printf("ASSOCIATE response: %v (len=%d)\n", resp2[:n], n)

	if n < 10 || resp2[1] != 0x00 {
		fmt.Println("UDP ASSOCIATE failed")
		return
	}

	// Parse BND.ADDR
	var relayIP net.IP
	var relayPort int
	atyp := resp2[3]
	switch atyp {
	case 0x01:
		relayIP = net.IP(resp2[4:8])
		relayPort = int(binary.BigEndian.Uint16(resp2[8:10]))
	}
	fmt.Printf("Relay: %s:%d\n", relayIP, relayPort)

	if relayIP == nil || relayIP.IsUnspecified() {
		host, _, _ := net.SplitHostPort(*proxyAddr)
		if host == "" {
			host = "127.0.0.1"
		}
		relayIP = net.ParseIP(host)
	}
	relayAddr := &net.UDPAddr{IP: relayIP, Port: relayPort}

	// 4. Create UDP socket and send to target via proxy relay
	udpConn, err := net.DialUDP("udp", nil, relayAddr)
	if err != nil {
		fmt.Println("UDP dial fail:", err)
		return
	}
	defer udpConn.Close()

	targetHost, targetPortStr, _ := net.SplitHostPort(*targetAddr)
	dstIP := net.ParseIP(targetHost)
	dstPort := uint16(9999)
	fmt.Sscanf(targetPortStr, "%d", &dstPort)
	payload := "TEST-7890-UDP"

	pkt := make([]byte, 0, 10+len(payload))
	pkt = append(pkt, 0x00, 0x00, 0x00, 0x01)
	pkt = append(pkt, dstIP.To4()...)
	portBytes := make([]byte, 2)
	binary.BigEndian.PutUint16(portBytes, dstPort)
	pkt = append(pkt, portBytes...)
	pkt = append(pkt, []byte(payload)...)

	fmt.Printf("Sending %d bytes to relay %s\n", len(pkt), relayAddr)
	udpConn.SetDeadline(time.Now().Add(10 * time.Second))
	udpConn.Write(pkt)

	buf := make([]byte, 4096)
	nr, from, err := udpConn.ReadFrom(buf)
	if err != nil {
		fmt.Println("UDP read fail:", err)
		return
	}
	fmt.Printf("SUCCESS: Received %d bytes from %v\n", nr, from)
}
