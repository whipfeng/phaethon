package dialer

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"

	"phaethon/util"
)

func TestEncodeTrojanAddr(t *testing.T) {
	tests := []struct {
		host     string
		port     int
		wantAtyp byte
	}{
		{"127.0.0.1", 8080, 0x01},
		{"::1", 8080, 0x04},
		{"example.com", 443, 0x03},
	}

	for _, tt := range tests {
		atyp, addrBytes, err := util.EncodeTrojanAddr(tt.host, tt.port)
		if err != nil {
			t.Errorf("encodeTrojanAddr(%q, %d) error: %v", tt.host, tt.port, err)
			continue
		}
		if atyp != tt.wantAtyp {
			t.Errorf("encodeTrojanAddr(%q, %d) atyp = %d, want %d", tt.host, tt.port, atyp, tt.wantAtyp)
		}
		// Verify port is in addrBytes
		port := binary.BigEndian.Uint16(addrBytes[len(addrBytes)-2:])
		if int(port) != tt.port {
			t.Errorf("encodeTrojanAddr(%q, %d) port = %d, want %d", tt.host, tt.port, port, tt.port)
		}
	}
}

func TestTrojanUDPPacketRoundTrip(t *testing.T) {
	// Create a mock TLS connection using net.Pipe
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	pc := &trojanPacketConn{tlsConn: clientConn, closed: make(chan struct{})}

	targetAddr, _ := net.ResolveUDPAddr("udp", "8.8.8.8:53")
	testData := []byte("hello udp")

	// Write in goroutine
	done := make(chan error, 1)
	go func() {
		_, err := pc.WriteTo(testData, targetAddr)
		done <- err
	}()

	// Read on server side
	atyp, err := util.ReadByte(serverConn)
	if err != nil {
		t.Fatal(err)
	}
	if atyp != 0x01 {
		t.Fatalf("atyp = %d, want 1", atyp)
	}

	ipBuf := make([]byte, 4)
	if _, err := io.ReadFull(serverConn, ipBuf); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ipBuf, net.ParseIP("8.8.8.8").To4()) {
		t.Fatalf("ip = %v", ipBuf)
	}

	port, err := util.ReadPort(serverConn)
	if err != nil {
		t.Fatal(err)
	}
	if port != 53 {
		t.Fatalf("port = %d, want 53", port)
	}

	length, err := util.ReadLength(serverConn)
	if err != nil {
		t.Fatal(err)
	}
	if length != len(testData) {
		t.Fatalf("length = %d, want %d", length, len(testData))
	}

	crlf := make([]byte, 2)
	if _, err := io.ReadFull(serverConn, crlf); err != nil {
		t.Fatal(err)
	}
	if crlf[0] != 0x0D || crlf[1] != 0x0A {
		t.Fatalf("crlf = %v", crlf)
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(serverConn, payload); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload, testData) {
		t.Fatalf("payload = %q, want %q", payload, testData)
	}

	if err := <-done; err != nil {
		t.Fatalf("WriteTo error: %v", err)
	}
}
