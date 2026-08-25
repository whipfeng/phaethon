package dialer

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"phaethon/util"
)

// HTTPDialer connects to a destination through an HTTP proxy using CONNECT tunneling.
type HTTPDialer struct {
	BaseDialer
}

func (d *HTTPDialer) Dial(dstAddr string, dstPort int) (net.Conn, error) {
	addr := net.JoinHostPort(d.Proxy.Server, strconv.Itoa(d.Proxy.Port))
	conn, err := net.DialTimeout("tcp", addr, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("http: connect to proxy fail: %w", err)
	}
	util.SetTCPNoDelay(conn)

	// Build CONNECT request manually
	targetAddr := net.JoinHostPort(dstAddr, strconv.Itoa(dstPort))
	reqLine := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", targetAddr, targetAddr)
	if d.Proxy.Username != "" {
		auth := base64.StdEncoding.EncodeToString([]byte(d.Proxy.Username + ":" + d.Proxy.Password))
		reqLine += "Proxy-Authorization: Basic " + auth + "\r\n"
	}
	reqLine += "\r\n"

	if _, err := conn.Write([]byte(reqLine)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("http: send CONNECT fail: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("http: read response fail: %w", err)
	}
	if resp.StatusCode != 200 {
		conn.Close()
		return nil, fmt.Errorf("http: CONNECT failed with status %d", resp.StatusCode)
	}

	// http.ReadResponse uses a bufio.Reader which may have buffered extra bytes
	// beyond the HTTP response. After a successful CONNECT, the connection is a
	// raw tunnel — preserve any buffered data.
	if br.Buffered() > 0 {
		prefix := make([]byte, br.Buffered())
		io.ReadFull(br, prefix)
		return &prefixConn{Conn: conn, reader: io.MultiReader(bytes.NewReader(prefix), conn)}, nil
	}
	return conn, nil
}

// prefixConn wraps a net.Conn, prepending buffered data to reads.
type prefixConn struct {
	net.Conn
	reader io.Reader
}

func (c *prefixConn) Read(b []byte) (int, error) {
	return c.reader.Read(b)
}
