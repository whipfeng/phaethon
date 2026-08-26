package dialer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"phaethon/config"
	"phaethon/util"
)

const (
	HeaderConnectionID = "X-I"
	HeaderTargetHost   = "X-H"
	HeaderTargetPort   = "X-P"
	HeaderContentSeq   = "X-S"
	HeaderCommand      = "X-C"

	// Backward-compatible aliases.
	headerConnectionID = HeaderConnectionID
	headerTargetHost   = HeaderTargetHost
	headerTargetPort   = HeaderTargetPort
	headerContentSeq   = HeaderContentSeq
	headerCommand      = HeaderCommand
)

// HTunnelDialer connects through an HTTP tunnel proxy
type HTunnelDialer struct {
	BaseDialer
}

// NewHTunnelHTTPClient creates an http.Client that dials through proxy.Next
// when a proxy chain is configured.
func NewHTunnelHTTPClient(proxy *config.Proxy) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				nextType := "nil"
				if proxy.Next != nil {
					nextType = proxy.Next.Type
				}
				if proxy.Next != nil && proxy.Next.Type != config.ProxyDIRECT {
					util.LogDebug("[HTUNNEL-DIAL] [%s] chaining via %s to %s:%d (URL addr=%s)", proxy.Name, nextType, proxy.Server, proxy.Port, addr)
					nextDialer := NewDialer(proxy.Next)
					return nextDialer.Dial(proxy.Server, proxy.Port)
				}
				util.LogDebug("[HTUNNEL-DIAL] [%s] direct to URL addr=%s (server=%s:%d, next=%s)", proxy.Name, addr, proxy.Server, proxy.Port, nextType)
				return DialRouteAware(network, addr)
			},
		},
	}
}

func (d *HTunnelDialer) Dial(dstAddr string, dstPort int) (net.Conn, error) {
	// If this proxy is configured as a reverse channel, obtain from registry
	if conn, err := d.TryReverse(); err != nil {
		return nil, fmt.Errorf("htunnel: %w", err)
	} else if conn != nil {
		return conn, nil
	}

	proxy := d.Proxy
	crypto := util.NewHTunnelCrypto(proxy.Password)

	encHost := crypto.SealHeader(dstAddr)
	encPort := crypto.SealHeader(strconv.Itoa(dstPort))

	connSeq := 0

	cmd := "CONN"
	if d.IsBind(dstPort) {
		cmd = "BIND"
	}

	// Step 1: Request connection ID (URL format matches Java: url + "//" + connSeq)
	req, _ := http.NewRequest("HEAD", fmt.Sprintf("%s//%d", proxy.URL, connSeq), nil)
	req.Header.Set(headerTargetHost, encHost)
	req.Header.Set(headerTargetPort, encPort)
	req.Header.Set(headerContentSeq, strconv.Itoa(connSeq))
	req.Header.Set(headerCommand, cmd)
	connSeq++

	client := NewHTunnelHTTPClient(proxy)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("htunnel: connection request fail: %w", err)
	}
	resp.Body.Close()
	cancel()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("htunnel: connection request status: %d", resp.StatusCode)
	}
	connectionID := resp.Header.Get(headerConnectionID)
	if connectionID == "" {
		return nil, fmt.Errorf("htunnel: no connection ID returned")
	}

	// Step 2: Push connection (URL format matches Java: url + "/" + connectionId + "/" + connSeq)
	req, _ = http.NewRequest("HEAD", fmt.Sprintf("%s/%s/%d", proxy.URL, connectionID, connSeq), nil)
	req.Header.Set(headerConnectionID, connectionID)
	req.Header.Set(headerContentSeq, strconv.Itoa(connSeq))
	connSeq++

	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	resp, err = client.Do(req.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("htunnel: push connect fail: %w", err)
	}
	resp.Body.Close()
	cancel()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("htunnel: push connect status: %d", resp.StatusCode)
	}

	// Step 3: Ack connection
	req, _ = http.NewRequest("HEAD", fmt.Sprintf("%s/%s/%d", proxy.URL, connectionID, connSeq), nil)
	req.Header.Set(headerConnectionID, connectionID)
	req.Header.Set(headerContentSeq, strconv.Itoa(connSeq))
	connSeq++

	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	resp, err = client.Do(req.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("htunnel: ack connect fail: %w", err)
	}
	resp.Body.Close()
	cancel()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("htunnel: ack connect status: %d", resp.StatusCode)
	}

	util.LogDebug("[HTUNNEL-CLI] [%s] [%s] Connecting %s:%d via %s (connectionID=%s)", proxy.Name, d.ConnIDStr(), dstAddr, dstPort, proxy.URL, connectionID)

	conn := &htunnelConn{
		proxy:        proxy,
		connectionID: connectionID,
		client:       client,
		connSeq:      connSeq,
		closed:       make(chan struct{}),
		crypto:       util.NewHTunnelCrypto(proxy.Password),
	}

	// Start heartbeat loop
	go conn.heartbeatLoop()

	return conn, nil
}

// htunnelConn implements net.Conn over HTTP tunnel
type htunnelConn struct {
	proxy        *config.Proxy
	connectionID string
	client       *http.Client
	connSeq      int
	readSeq      int
	writeSeq     int

	readBuf    []byte
	readOffset int
	readMu     sync.Mutex
	writeMu    sync.Mutex
	seqMu      sync.Mutex

	// AEAD encryption (XChaCha20-Poly1305, nil when no password)
	crypto *util.HTunnelCrypto

	closed    chan struct{}
	closeOnce sync.Once
}

func (c *htunnelConn) Read(b []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()

	// If we have buffered data, return that first
	if c.readOffset < len(c.readBuf) {
		n := copy(b, c.readBuf[c.readOffset:])
		c.readOffset += n
		return n, nil
	}

	// Fetch new data from server
	for {
		select {
		case <-c.closed:
			return 0, io.EOF
		default:
		}

		c.seqMu.Lock()
		c.readSeq++
		readSeq := c.readSeq
		c.seqMu.Unlock()

		url := fmt.Sprintf("%s/%s/%d", c.proxy.URL, c.connectionID, readSeq)
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set(headerConnectionID, c.connectionID)
		req.Header.Set(headerContentSeq, strconv.Itoa(readSeq))

		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		resp, err := c.client.Do(req.WithContext(ctx))
		if err != nil {
			cancel()
			return 0, fmt.Errorf("htunnel: read fail: %w", err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		if err != nil {
			return 0, fmt.Errorf("htunnel: read body fail: %w", err)
		}

		if resp.StatusCode == 410 {
			return 0, io.ErrClosedPipe
		}
		if resp.StatusCode == 408 {
			// Timeout, retry
			continue
		}
		if resp.StatusCode != 200 {
			return 0, fmt.Errorf("htunnel: read status: %d", resp.StatusCode)
		}

		if len(body) > 0 {
			if c.crypto.IsEnabled() {
				var err error
				body, err = c.crypto.OpenBody(body)
				if err != nil {
					return 0, fmt.Errorf("htunnel: decrypt fail: %w", err)
				}
			}
			c.readBuf = body
			c.readOffset = 0
			n := copy(b, body)
			c.readOffset = n
			return n, nil
		}
	}
}

func (c *htunnelConn) Write(b []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	select {
	case <-c.closed:
		return 0, io.ErrClosedPipe
	default:
	}

	data := c.crypto.SealBody(b)

	c.seqMu.Lock()
	c.writeSeq++
	writeSeq := c.writeSeq
	c.seqMu.Unlock()

	url := fmt.Sprintf("%s/%s/%d", c.proxy.URL, c.connectionID, writeSeq)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(data))
	req.Header.Set(headerConnectionID, c.connectionID)
	req.Header.Set(headerContentSeq, strconv.Itoa(writeSeq))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	resp, err := c.client.Do(req.WithContext(ctx))
	if err != nil {
		cancel()
		return 0, fmt.Errorf("htunnel: write fail: %w", err)
	}
	resp.Body.Close()
	cancel()

	if resp.StatusCode == 410 {
		return 0, io.ErrClosedPipe
	}
	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("htunnel: write status: %d", resp.StatusCode)
	}

	return len(b), nil
}

func (c *htunnelConn) heartbeatLoop() {
	// Initial delay before first heartbeat
	select {
	case <-c.closed:
		return
	case <-time.After(30 * time.Second):
	}

	for {
		select {
		case <-c.closed:
			return
		default:
		}

		c.seqMu.Lock()
		connSeq := c.connSeq
		c.connSeq++
		c.seqMu.Unlock()

		url := fmt.Sprintf("%s/%s/%d", c.proxy.URL, c.connectionID, connSeq)
		req, _ := http.NewRequest("PUT", url, nil)
		req.Header.Set(headerConnectionID, c.connectionID)
		req.Header.Set(headerContentSeq, strconv.Itoa(connSeq))

		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		resp, err := c.client.Do(req.WithContext(ctx))
		if err != nil {
			cancel()
			util.LogWarn("[HTUNNEL-CLI] [%s] heartbeat PUT fail (seq=%d): %T %v", c.proxy.Name, connSeq, err, err)
			c.Close()
			return
		}
		resp.Body.Close()
		cancel()

		if resp.StatusCode == 410 {
			util.LogWarn("[HTUNNEL-CLI] [%s] heartbeat PUT 410 (seq=%d): server closed channel", c.proxy.Name, connSeq)
			c.Close()
			return
		}
		if resp.StatusCode != 200 {
			util.LogWarn("[HTUNNEL-CLI] [%s] heartbeat PUT status=%d (seq=%d): unexpected", c.proxy.Name, resp.StatusCode, connSeq)
			c.Close()
			return
		}

		timer := time.NewTimer(30 * time.Second)
		select {
		case <-c.closed:
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (c *htunnelConn) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)

		c.seqMu.Lock()
		connSeq := c.connSeq
		c.connSeq++
		c.seqMu.Unlock()

		url := fmt.Sprintf("%s/%s/%d", c.proxy.URL, c.connectionID, connSeq)
		req, _ := http.NewRequest("DELETE", url, nil)
		req.Header.Set(headerConnectionID, c.connectionID)
		req.Header.Set(headerContentSeq, strconv.Itoa(connSeq))

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		resp, err := c.client.Do(req.WithContext(ctx))
		if err == nil {
			resp.Body.Close()
		}
		cancel()
	})
	return nil
}

func (c *htunnelConn) LocalAddr() net.Addr                { return &net.TCPAddr{} }
func (c *htunnelConn) RemoteAddr() net.Addr               { return &net.TCPAddr{} }
func (c *htunnelConn) SetDeadline(t time.Time) error      { return nil }
func (c *htunnelConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *htunnelConn) SetWriteDeadline(t time.Time) error { return nil }

func (c *htunnelConn) CloseWrite() error {
	return nil
}

// ---------------------------------------------------------------------------
// UDP support for h_tunnel
// ---------------------------------------------------------------------------

func (d *HTunnelDialer) DialPacket() (net.PacketConn, error) {
	proxy := d.Proxy
	crypto := util.NewHTunnelCrypto(proxy.Password)

	encHost := crypto.SealHeader("0.0.0.0")
	encPort := crypto.SealHeader("0")
	connSeq := 0

	// Step 1: Request connection ID with UDP command
	req, _ := http.NewRequest("HEAD", fmt.Sprintf("%s//%d", proxy.URL, connSeq), nil)
	req.Header.Set(headerTargetHost, encHost)
	req.Header.Set(headerTargetPort, encPort)
	req.Header.Set(headerContentSeq, strconv.Itoa(connSeq))
	req.Header.Set(headerCommand, "UDP")
	connSeq++

	client := NewHTunnelHTTPClient(proxy)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("htunnel-udp: connection request fail: %w", err)
	}
	resp.Body.Close()
	cancel()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("htunnel-udp: connection request status: %d", resp.StatusCode)
	}
	connectionID := resp.Header.Get(headerConnectionID)
	if connectionID == "" {
		return nil, fmt.Errorf("htunnel-udp: no connection ID returned")
	}

	// Step 2: Push connection
	req, _ = http.NewRequest("HEAD", fmt.Sprintf("%s/%s/%d", proxy.URL, connectionID, connSeq), nil)
	req.Header.Set(headerConnectionID, connectionID)
	req.Header.Set(headerContentSeq, strconv.Itoa(connSeq))
	connSeq++

	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	resp, err = client.Do(req.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("htunnel-udp: push connect fail: %w", err)
	}
	resp.Body.Close()
	cancel()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("htunnel-udp: push connect status: %d", resp.StatusCode)
	}

	// Step 3: Ack connection
	req, _ = http.NewRequest("HEAD", fmt.Sprintf("%s/%s/%d", proxy.URL, connectionID, connSeq), nil)
	req.Header.Set(headerConnectionID, connectionID)
	req.Header.Set(headerContentSeq, strconv.Itoa(connSeq))
	connSeq++

	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	resp, err = client.Do(req.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("htunnel-udp: ack connect fail: %w", err)
	}
	resp.Body.Close()
	cancel()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("htunnel-udp: ack connect status: %d", resp.StatusCode)
	}

	util.LogDebug("[HTUNNEL-CLI] [%s] [%s] UDP ASSOCIATE via %s (connectionID=%s)", proxy.Name, d.ConnIDStr(), proxy.URL, connectionID)

	conn := &htunnelPacketConn{
		proxy:        proxy,
		connectionID: connectionID,
		client:       client,
		connSeq:      connSeq,
		closed:       make(chan struct{}),
		crypto:       util.NewHTunnelCrypto(proxy.Password),
	}

	// Start heartbeat loop for NAT keepalive
	go conn.heartbeatLoop()

	return conn, nil
}

// htunnelPacketConn implements net.PacketConn over h_tunnel.
type htunnelPacketConn struct {
	proxy        *config.Proxy
	connectionID string
	client       *http.Client
	connSeq      int
	readSeq      int
	writeSeq     int

	readMu  sync.Mutex
	writeMu sync.Mutex
	seqMu   sync.Mutex

	crypto *util.HTunnelCrypto

	closed    chan struct{}
	closeOnce sync.Once
}

func (c *htunnelPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()

	for {
		select {
		case <-c.closed:
			return 0, nil, io.EOF
		default:
		}

		c.seqMu.Lock()
		c.readSeq++
		readSeq := c.readSeq
		c.seqMu.Unlock()

		url := fmt.Sprintf("%s/%s/%d", c.proxy.URL, c.connectionID, readSeq)
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set(headerConnectionID, c.connectionID)
		req.Header.Set(headerContentSeq, strconv.Itoa(readSeq))

		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		resp, err := c.client.Do(req.WithContext(ctx))
		if err != nil {
			cancel()
			return 0, nil, fmt.Errorf("htunnel-udp: read fail: %w", err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		if err != nil {
			return 0, nil, fmt.Errorf("htunnel-udp: read body fail: %w", err)
		}

		if resp.StatusCode == 410 {
			return 0, nil, io.ErrClosedPipe
		}
		if resp.StatusCode == 408 {
			continue
		}
		if resp.StatusCode != 200 {
			return 0, nil, fmt.Errorf("htunnel-udp: read status: %d", resp.StatusCode)
		}

		if len(body) == 0 {
			continue
		}

		if c.crypto.IsEnabled() {
			var err error
			body, err = c.crypto.OpenBody(body)
			if err != nil {
				continue
			}
		}

		srcAddr, data, err := util.ParseUDPAddrHeader(body)
		if err != nil {
			continue
		}

		n := copy(b, data)
		return n, srcAddr, nil
	}
}

func (c *htunnelPacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	select {
	case <-c.closed:
		return 0, io.ErrClosedPipe
	default:
	}

	header, err := util.BuildUDPAddrHeader(addr)
	if err != nil {
		return 0, fmt.Errorf("htunnel-udp: build addr header fail: %w", err)
	}

	combined := make([]byte, len(header)+len(b))
	copy(combined, header)
	copy(combined[len(header):], b)
	data := c.crypto.SealBody(combined)

	c.seqMu.Lock()
	c.writeSeq++
	writeSeq := c.writeSeq
	c.seqMu.Unlock()

	url := fmt.Sprintf("%s/%s/%d", c.proxy.URL, c.connectionID, writeSeq)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(data))
	req.Header.Set(headerConnectionID, c.connectionID)
	req.Header.Set(headerContentSeq, strconv.Itoa(writeSeq))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	resp, err := c.client.Do(req.WithContext(ctx))
	if err != nil {
		cancel()
		return 0, fmt.Errorf("htunnel-udp: write fail: %w", err)
	}
	resp.Body.Close()
	cancel()

	if resp.StatusCode == 410 {
		return 0, io.ErrClosedPipe
	}
	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("htunnel-udp: write status: %d", resp.StatusCode)
	}

	return len(b), nil
}

func (c *htunnelPacketConn) heartbeatLoop() {
	// Initial delay before first heartbeat
	select {
	case <-c.closed:
		return
	case <-time.After(30 * time.Second):
	}

	for {
		select {
		case <-c.closed:
			return
		default:
		}

		c.seqMu.Lock()
		connSeq := c.connSeq
		c.connSeq++
		c.seqMu.Unlock()

		url := fmt.Sprintf("%s/%s/%d", c.proxy.URL, c.connectionID, connSeq)
		req, _ := http.NewRequest("PUT", url, nil)
		req.Header.Set(headerConnectionID, c.connectionID)
		req.Header.Set(headerContentSeq, strconv.Itoa(connSeq))

		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		resp, err := c.client.Do(req.WithContext(ctx))
		if err != nil {
			cancel()
			util.LogWarn("[HTUNNEL-CLI] [%s] UDP heartbeat PUT fail (seq=%d): %T %v", c.proxy.Name, connSeq, err, err)
			c.Close()
			return
		}
		resp.Body.Close()
		cancel()

		if resp.StatusCode == 410 {
			util.LogWarn("[HTUNNEL-CLI] [%s] UDP heartbeat PUT 410 (seq=%d): server closed channel", c.proxy.Name, connSeq)
			c.Close()
			return
		}
		if resp.StatusCode != 200 {
			util.LogWarn("[HTUNNEL-CLI] [%s] UDP heartbeat PUT status=%d (seq=%d): unexpected", c.proxy.Name, resp.StatusCode, connSeq)
			c.Close()
			return
		}

		timer := time.NewTimer(30 * time.Second)
		select {
		case <-c.closed:
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (c *htunnelPacketConn) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)

		c.seqMu.Lock()
		connSeq := c.connSeq
		c.connSeq++
		c.seqMu.Unlock()

		url := fmt.Sprintf("%s/%s/%d", c.proxy.URL, c.connectionID, connSeq)
		req, _ := http.NewRequest("DELETE", url, nil)
		req.Header.Set(headerConnectionID, c.connectionID)
		req.Header.Set(headerContentSeq, strconv.Itoa(connSeq))

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		resp, err := c.client.Do(req.WithContext(ctx))
		if err == nil {
			resp.Body.Close()
		}
		cancel()
	})
	return nil
}

func (c *htunnelPacketConn) LocalAddr() net.Addr                { return &net.UDPAddr{} }
func (c *htunnelPacketConn) SetDeadline(t time.Time) error      { return nil }
func (c *htunnelPacketConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *htunnelPacketConn) SetWriteDeadline(t time.Time) error { return nil }
