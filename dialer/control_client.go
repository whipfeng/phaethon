package dialer

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"phaethon/config"
	"phaethon/reverse"
	"phaethon/util"
)

// ControlClient manages the control connection from the reverse side.
// It connects to the registry using the specified protocol's BIND capability,
// registers for resources, and manages data connections.
type ControlClient struct {
	proxy         *config.Proxy
	registerAddr  string
	registerProto string
	ctrlConn      net.Conn
	address       string
	mu            sync.Mutex
	closeCh       chan struct{}
}

func NewControlClient(proxy *config.Proxy, registerAddr, registerProto string) *ControlClient {
	return &ControlClient{
		proxy:         proxy,
		registerAddr:  registerAddr,
		registerProto: registerProto,
		closeCh:       make(chan struct{}),
	}
}

// Connect establishes the control connection to the registry.
// Requires a non-nil outbound proxy — reverse clients must always go through a proxy chain.
func (c *ControlClient) Connect() error {
	if c.proxy == nil {
		return fmt.Errorf("control client: outbound proxy is required (reverse must connect through proxy)")
	}

	var conn net.Conn
	var err error

	switch c.registerProto {
	case "socks5":
		conn, err = c.connectSOCKS5()
	case "trojan":
		d := &TrojanDialer{BaseDialer: BaseDialer{Proxy: c.proxy}}
		nextDialer := NewDialer(c.proxy.Next)
		rawConn, err := nextDialer.Dial(c.proxy.Server, c.proxy.Port)
		if err != nil {
			return fmt.Errorf("trojan: connect fail: %w", err)
		}
		tlsConn, err := d.TLSHandshake(rawConn)
		if err != nil {
			rawConn.Close()
			return fmt.Errorf("trojan: tls fail: %w", err)
		}
		if err := d.SendTrojanRequestWithCmd(tlsConn, 0x02, c.registerAddr, 1); err != nil {
			tlsConn.Close()
			return fmt.Errorf("trojan: bind fail: %w", err)
		}
		conn = tlsConn
	case "h_tunnel":
		d := &HTunnelDialer{BaseDialer: BaseDialer{Proxy: c.proxy}}
		conn, err = d.Dial(c.registerAddr, 1)
		if err != nil {
			return fmt.Errorf("htunnel: dial fail: %w", err)
		}
	default:
		return fmt.Errorf("control client: unsupported register protocol: %s (only socks5/trojan/h_tunnel support BIND)", c.registerProto)
	}

	if err != nil {
		return fmt.Errorf("control client: connect fail (%s): %w", c.registerProto, err)
	}

	c.mu.Lock()
	c.ctrlConn = conn
	c.mu.Unlock()

	util.LogInfo("[CONTROL-CLIENT] connected to %s via %s as control channel", c.registerAddr, c.registerProto)
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
				util.LogInfo("[CONTROL-CLIENT] heartbeat send fail to %s: %v", c.registerAddr, err)
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
				util.LogInfo("[CONTROL-CLIENT] monitor detected disconnect from %s: %v", c.registerAddr, err)
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

// connectSOCKS5 dials to the registry through the proxy chain (using actual port, not 0),
// then sends SOCKS5 BIND(PORT=1) to declare this as a control channel.
func (c *ControlClient) connectSOCKS5() (net.Conn, error) {
	regHost, regPortStr, _ := net.SplitHostPort(c.registerAddr)
	regPort, _ := strconv.Atoi(regPortStr)
	if regPort <= 0 {
		regPort = 19901
	}
	conn, err := ChainDial(c.proxy, regHost, regPort)
	if err != nil {
		return nil, err
	}
	if err := sendSocks5BindControl(conn); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// sendSocks5BindControl performs a minimal SOCKS5 handshake followed by
// BIND(PORT=1) to declare a control connection.
func sendSocks5BindControl(conn net.Conn) error {
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	defer conn.SetWriteDeadline(time.Time{})

	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return err
	}

	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return err
	}
	if resp[0] != 0x05 || resp[1] != 0x00 {
		return fmt.Errorf("socks5 auth rejected: %v", resp)
	}

	addr := "control"
	addrLen := byte(len(addr))
	bindReq := []byte{0x05, 0x02, 0x00, 0x03, addrLen}
	bindReq = append(bindReq, []byte(addr)...)
	portBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(portBuf, 1)
	bindReq = append(bindReq, portBuf...)

	if _, err := conn.Write(bindReq); err != nil {
		return err
	}

	bindResp := make([]byte, 10)
	if _, err := io.ReadFull(conn, bindResp); err != nil {
		return err
	}
	if bindResp[1] != 0x00 {
		return fmt.Errorf("socks5 bind rejected: status=%d", bindResp[1])
	}

	return nil
}
