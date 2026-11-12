package dialer_test

import (
	"io"
	"net"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"phaethon/config"
	"phaethon/dialer"
	"phaethon/server"
	"phaethon/util"
)

// TestWSTargetCarrier verifies that a normal (non-mesh) proxy connection can
// ride the WebSocket carrier end-to-end: client Dial -> WS target handshake ->
// server dials the target -> byte splice.
func TestWSTargetCarrier(t *testing.T) {
	os.Setenv("PHAETHON_HT_WS", "all")
	defer os.Unsetenv("PHAETHON_HT_WS")

	// Target server: sends "hello" then closes.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		c.Write([]byte("hello"))
		time.Sleep(50 * time.Millisecond)
		c.Close()
	}()

	srv := &server.HTunnelServer{
		BaseServer: server.BaseServer{Mapping: &config.Mapping{Name: "t"}},
		Password:   "pw",
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	proxy := &config.Proxy{
		Name:     "t",
		Type:     config.ProxyH_TUNNEL,
		URL:      ts.URL,
		Password: "pw",
	}
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	var port int
	for _, ch := range portStr {
		port = port*10 + int(ch-'0')
	}

	conn, err := dialer.NewDialer(proxy).Dial(host, port)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 16)
	n, err := conn.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("read: %v", err)
	}
	if string(buf[:n]) != "hello" {
		t.Fatalf("got %q, want hello", buf[:n])
	}
}

// TestWSBindCarrierControl verifies that a BIND channel (control, PORT=1) can
// ride the WebSocket carrier: the client upgrades with X-C: BIND, then reads
// and writes a sealed byte stream.
func TestWSBindCarrierControl(t *testing.T) {
	os.Setenv("PHAETHON_HT_WS", "all")
	defer os.Unsetenv("PHAETHON_HT_WS")

	crypto := util.NewHTunnelCrypto("pw")
	wsSrv := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
		for {
			var msg []byte
			if err := websocket.Message.Receive(ws, &msg); err != nil {
				return
			}
			plain, err := crypto.OpenBody(msg)
			if err != nil {
				return
			}
			if err := websocket.Message.Send(ws, crypto.SealBody(append([]byte("echo:"), plain...))); err != nil {
				return
			}
		}
	}))
	defer wsSrv.Close()

	proxy := &config.Proxy{
		Name:     "t-bind",
		Type:     config.ProxyH_TUNNEL,
		URL:      wsSrv.URL,
		Server:   "127.0.0.1",
		Password: "pw",
	}
	d := &dialer.HTunnelDialer{BaseDialer: dialer.BaseDialer{Proxy: proxy}}
	conn, err := d.DialControl()
	if err != nil {
		t.Fatalf("DialControl: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 32)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf[:n]) != "echo:ping" {
		t.Fatalf("got %q, want echo:ping", buf[:n])
	}
	if label := dialer.HTCarrierLabel(proxy); label != "WS" {
		t.Fatalf("carrier label = %q, want WS", label)
	}
}
