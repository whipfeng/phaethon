package dialer

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"phaethon/config"
	"phaethon/reverse"
	"phaethon/util"
)

// ControlClient manages the control connection from the reverse side.
// It connects to the registry using the outbound proxy's DialControl method,
// registers for resources, and manages data connections.
type ControlClient struct {
	proxy    *config.Proxy
	ctrlConn net.Conn
	address  string
	mu       sync.Mutex
	closeCh  chan struct{}
}

func NewControlClient(proxy *config.Proxy) *ControlClient {
	return &ControlClient{
		proxy:   proxy,
		closeCh: make(chan struct{}),
	}
}

// registryAddr returns the registry address derived from the outbound proxy.
func (c *ControlClient) registryAddr() string {
	return fmt.Sprintf("%s:%d", c.proxy.Server, c.proxy.Port)
}

// Connect establishes the control connection to the registry.
// The outbound proxy IS the registry: it delegates to the proxy type's DialControl
// method, which handles protocol-specific handshake with PORT=1 (control channel).
func (c *ControlClient) Connect() error {
	if c.proxy == nil {
		return fmt.Errorf("control client: outbound proxy is required (reverse must connect through proxy)")
	}

	d := NewDialer(c.proxy)
	cd, ok := d.(ControlDialer)
	if !ok {
		return fmt.Errorf("control client: proxy type %s does not support control connections (only socks5/trojan/h_tunnel)", c.proxy.Type)
	}

	conn, err := cd.DialControl()
	if err != nil {
		return fmt.Errorf("control client: connect fail (%s): %w", c.proxy.Type, err)
	}

	c.mu.Lock()
	c.ctrlConn = conn
	c.mu.Unlock()

	util.LogInfo("[CONTROL-CLIENT] connected to %s via %s as control channel", c.registryAddr(), c.proxy.Type)
	return nil
}

// Register sends a registration request with full listener configuration
// (protocol type, credentials, direct target info) over the control connection.
func (c *ControlClient) Register(req reverse.ControlRequest) (*reverse.ControlReply, error) {
	c.mu.Lock()
	conn := c.ctrlConn
	addr := c.address
	c.mu.Unlock()

	if conn == nil {
		return nil, fmt.Errorf("control connection not established (addr=%q)", addr)
	}

	reqBytes, _ := json.Marshal(req)

	if err := reverse.WriteFrame(conn, reverse.FrameData, reqBytes); err != nil {
		return nil, fmt.Errorf("send register fail: %w", err)
	}

	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	frameType, payload, err := reverse.ReadFrame(conn)
	if err != nil {
		return nil, fmt.Errorf("read register reply fail: %w", err)
	}
	conn.SetReadDeadline(time.Time{})

	if frameType != reverse.FrameData || len(payload) == 0 {
		return nil, fmt.Errorf("unexpected reply frame type: 0x%02x", frameType)
	}

	var reply reverse.ControlReply
	if err := json.Unmarshal(payload, &reply); err != nil {
		return nil, fmt.Errorf("parse register reply fail: %w", err)
	}

	if reply.Status == "ok" {
		c.mu.Lock()
		c.address = reply.Address
		c.mu.Unlock()

		util.LogInfo("[CONTROL-CLIENT] registered: proto=%s, port=%d, address=%s",
			req.Proto, reply.Port, reply.Address)
	} else {
		util.LogInfo("[CONTROL-CLIENT] register rejected: status=%s error=%s",
			reply.Status, reply.Error)
	}

	return &reply, nil
}

// Keepalive sends heartbeats on the control connection.
// If a send fails, Close() is called so that Done() is signaled and the
// caller (runReverseSession) can reconnect.
func (c *ControlClient) Keepalive() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-c.closeCh:
			return
		case <-ticker.C:
			c.mu.Lock()
			conn := c.ctrlConn
			c.mu.Unlock()

			if conn == nil {
				return
			}
			if err := reverse.WriteFrame(conn, reverse.FrameHeartbeat, nil); err != nil {
				util.LogInfo("[CONTROL-CLIENT] heartbeat send fail to %s: %v", c.registryAddr(), err)
				c.Close()
				return
			}
		}
	}
}

// StartMonitor starts a background goroutine that reads from the control
// connection. When the remote side closes the connection, ReadFrame returns
// an error and Close() is called to signal Done(). This ensures the reverse
// client detects control-channel loss immediately instead of hanging forever.
func (c *ControlClient) StartMonitor() {
	go func() {
		for {
			c.mu.Lock()
			conn := c.ctrlConn
			c.mu.Unlock()
			if conn == nil {
				return
			}
			// Deadline slightly longer than the registry heartbeat interval (30s)
			conn.SetReadDeadline(time.Now().Add(70 * time.Second))
			_, _, err := reverse.ReadFrame(conn)
			if err != nil {
				util.LogInfo("[CONTROL-CLIENT] monitor detected disconnect from %s: %v", c.registryAddr(), err)
				c.Close()
				return
			}
		}
	}()
}

func (c *ControlClient) Address() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.address
}

func (c *ControlClient) Done() <-chan struct{} {
	return c.closeCh
}

func (c *ControlClient) Close() {
	select {
	case <-c.closeCh:
	default:
		close(c.closeCh)
	}

	c.mu.Lock()
	if c.ctrlConn != nil {
		c.ctrlConn.Close()
		c.ctrlConn = nil
	}
	c.mu.Unlock()
}
