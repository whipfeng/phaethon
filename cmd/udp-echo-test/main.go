package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net"
	"time"
)

func containsPayload(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > len(sub) && s[len(s)-len(sub):] == sub)
}

func main() {
	socksAddr := flag.String("proxy", "127.0.0.1:7890", "SOCKS5 proxy address")
	targetHost := flag.String("target", "127.0.0.1:9999", "Target host:port")
	flag.Parse()

	// Parse target
	targetIP, targetPortStr, err := net.SplitHostPort(*targetHost)
	if err != nil {
		log.Fatal("Invalid target:", err)
	}
	targetPort := 0
	fmt.Sscanf(targetPortStr, "%d", &targetPort)

	// Step 1: TCP connect to SOCKS5 proxy
	tcpConn, err := net.DialTimeout("tcp", *socksAddr, 10*time.Second)
	if err != nil {
		log.Fatal("TCP connect to SOCKS5:", err)
	}
	defer tcpConn.Close()
	fmt.Println("Connected to SOCKS5 proxy at", *socksAddr)

	// Step 2: SOCKS5 handshake (no auth)
	tcpConn.Write([]byte{0x05, 0x01, 0x00})
	resp := make([]byte, 2)
	if _, err := tcpConn.Read(resp); err != nil {
		log.Fatal("SOCKS5 greeting read:", err)
	}
	fmt.Printf("SOCKS5 version=%d method=%d\n", resp[0], resp[1])

	// Step 3: UDP ASSOCIATE request (DST.ADDR = 0.0.0.0:0)
	req := []byte{0x05, 0x03, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
	tcpConn.Write(req)

	resp2 := make([]byte, 262)
	n, err := tcpConn.Read(resp2)
	if err != nil {
		log.Fatal("SOCKS5 reply read:", err)
	}
	if n < 10 || resp2[1] != 0x00 {
		log.Fatalf("SOCKS5 UDP ASSOCIATE failed: rep=%d", resp2[1])
	}

	var relayIP net.IP
	var relayPort int
	atyp := resp2[3]
	switch atyp {
	case 0x01:
		relayIP = net.IP(resp2[4:8])
		relayPort = int(binary.BigEndian.Uint16(resp2[8:10]))
	case 0x03:
		domainLen := int(resp2[4])
		domain := string(resp2[5 : 5+domainLen])
		fmt.Printf("Relay domain: %s\n", domain)
		ips, _ := net.LookupIP(domain)
		if len(ips) == 0 {
			log.Fatal("cannot resolve relay domain:", domain)
		}
		relayIP = ips[0]
		relayPort = int(binary.BigEndian.Uint16(resp2[5+domainLen : 7+domainLen]))
	case 0x04:
		relayIP = net.IP(resp2[4:20])
		relayPort = int(binary.BigEndian.Uint16(resp2[20:22]))
	}
	fmt.Printf("UDP relay: %s:%d\n", relayIP, relayPort)

	// Handle BND.ADDR = 0.0.0.0 — substitute with proxy server IP
	if relayIP == nil || relayIP.IsUnspecified() {
		proxyHost, _, _ := net.SplitHostPort(*socksAddr)
		relayIP = net.ParseIP(proxyHost)
		if relayIP == nil || relayIP.IsUnspecified() {
			relayIP = net.ParseIP("127.0.0.1")
		}
		fmt.Printf("BND.ADDR is 0.0.0.0, using %s\n", relayIP)
	}
	relayAddr := &net.UDPAddr{IP: relayIP, Port: relayPort}

	// Step 4: Create UDP connection to relay
	udpConn, err := net.DialUDP("udp", nil, relayAddr)
	if err != nil {
		log.Fatal("UDP dial relay:", err)
	}
	defer udpConn.Close()

	// Step 5: Send echo payload to target via SOCKS5 UDP relay
	dstIP := net.ParseIP(targetIP)
	dstPort := uint16(targetPort)

	payloadID := uint32(rand.Intn(999999))
	payload := fmt.Sprintf("HELLO-UDP-ECHO-%d", payloadID)
	fmt.Printf("Target: %s:%d\n", targetIP, targetPort)
	fmt.Printf("Payload: %s\n", payload)

	pkt := make([]byte, 0, 10+len(payload))
	pkt = append(pkt, 0x00, 0x00) // RSV (2 bytes)
	pkt = append(pkt, 0x00)       // FRAG = 0
	pkt = append(pkt, 0x01)       // ATYP = IPv4
	pkt = append(pkt, dstIP.To4()...)
	portBytes := make([]byte, 2)
	binary.BigEndian.PutUint16(portBytes, dstPort)
	pkt = append(pkt, portBytes...)
	pkt = append(pkt, []byte(payload)...)

	fmt.Printf("Sending %d bytes to %s:%d via SOCKS5 UDP relay\n", len(pkt), targetIP, targetPort)

	udpConn.SetDeadline(time.Now().Add(15 * time.Second))
	nw, err := udpConn.Write(pkt)
	if err != nil {
		log.Fatal("UDP write:", err)
	}
	fmt.Printf("Sent %d bytes (incl SOCKS5 header)\n", nw)

	// Step 6: Read reply
	buf := make([]byte, 4096)
	nr, from, err := udpConn.ReadFrom(buf)
	if err != nil {
		log.Fatal("UDP read:", err)
	}

	fmt.Printf("Received %d bytes from %v\n", nr, from)

	// Parse SOCKS5 UDP reply header
	replyData := buf[:nr]
	if len(replyData) < 10 {
		log.Fatalf("Reply too short: %d bytes", len(replyData))
	}
	replyAtyp := replyData[3]
	offset := 4 // RSV(2)+FRAG(1)+ATYP(1)
	switch replyAtyp {
	case 0x01:
		offset += 4
	case 0x03:
		offset += 1 + int(replyData[4])
	case 0x04:
		offset += 16
	}
	offset += 2 // port
	echoPayload := replyData[offset:]

	fmt.Printf("Echo response: %q (%d bytes)\n", string(echoPayload), len(echoPayload))
	if string(echoPayload) == payload || containsPayload(string(echoPayload), payload) {
		fmt.Println("\n✓ SUCCESS: First UDP round-trip works!")
	} else {
		fmt.Printf("\n✗ MISMATCH: got %q but expected %q\n", string(echoPayload), payload)
		return
	}

	// Step 7: Wait 35 seconds and send again
	fmt.Println("\nWaiting 35 seconds for silence test...")
	time.Sleep(35 * time.Second)

	payloadID2 := uint32(rand.Intn(999999))
	payload2 := fmt.Sprintf("SILENCE-TEST-%d", payloadID2)
	fmt.Printf("\nPayload after silence: %s\n", payload2)

	pkt2 := make([]byte, 0, 10+len(payload2))
	pkt2 = append(pkt2, 0x00, 0x00)
	pkt2 = append(pkt2, 0x00)
	pkt2 = append(pkt2, 0x01)
	pkt2 = append(pkt2, dstIP.To4()...)
	binary.BigEndian.PutUint16(portBytes, dstPort)
	pkt2 = append(pkt2, portBytes...)
	pkt2 = append(pkt2, []byte(payload2)...)

	udpConn.SetDeadline(time.Now().Add(15 * time.Second))
	nw2, err := udpConn.Write(pkt2)
	if err != nil {
		log.Fatal("UDP write after silence:", err)
	}
	fmt.Printf("Sent %d bytes after silence\n", nw2)

	buf2 := make([]byte, 4096)
	nr2, from2, err := udpConn.ReadFrom(buf2)
	if err != nil {
		log.Fatal("UDP read after silence:", err)
	}
	fmt.Printf("Received %d bytes from %v after silence\n", nr2, from2)

	replyData2 := buf2[:nr2]
	offset2 := 4
	replyAtyp2 := replyData2[3]
	switch replyAtyp2 {
	case 0x01:
		offset2 += 4
	case 0x03:
		offset2 += 1 + int(replyData2[4])
	case 0x04:
		offset2 += 16
	}
	offset2 += 2
	echoPayload2 := replyData2[offset2:]

	fmt.Printf("Echo response after silence: %q\n", string(echoPayload2))
	if string(echoPayload2) == payload2 || containsPayload(string(echoPayload2), payload2) {
		fmt.Println("\n✓✓✓ FULL SUCCESS: UDP works after 35s silence!")
	} else {
		fmt.Printf("\n✗✗✗ FAIL after silence: got %q expected %q\n", string(echoPayload2), payload2)
	}
}
