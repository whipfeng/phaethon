package dialer

import (
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/apernet/hysteria/core/v2/client"
	"phaethon/config"
	"phaethon/util"
)

var (
	hyPoolInstance *hysteria2Pool
	hyPoolOnce     sync.Once
)

func getHysteria2Pool() *hysteria2Pool {
	hyPoolOnce.Do(func() {
		hyPoolInstance = &hysteria2Pool{
			clients: make(map[string]*refClient),
		}
	})
	return hyPoolInstance
}

type refClient struct {
	client client.Client
	refCnt int
}

type hysteria2Pool struct {
	clients map[string]*refClient
	mu      sync.Mutex
}

func (p *hysteria2Pool) getClient(name string, proxy *config.Proxy) (client.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if refc, ok := p.clients[name]; ok {
		refc.refCnt++
		return refc.client, nil
	}

	hyConfig := &client.Config{}
	hostPort := net.JoinHostPort(proxy.Server, strconv.Itoa(proxy.Port))
	addr, err := net.ResolveUDPAddr("udp", hostPort)
	if err != nil {
		return nil, err
	}
	hyConfig.ServerAddr = addr
	sni := proxy.Sni
	if sni == "" {
		sni = proxy.Servername
	}
	if sni == "" {
		sni = proxy.Server
	}
	hyConfig.TLSConfig.ServerName = sni
	if proxy.Next != nil && proxy.Next.Type == config.ProxySOCKS5 {
		hyConfig.ConnFactory = &socks5UDPConnFactory{proxy: proxy.Next}
	} else {
		hyConfig.ConnFactory = &udpConnFactory{}
	}
	hyConfig.Auth = proxy.Password
	hyConfig.TLSConfig.InsecureSkipVerify = proxy.SkipCertVerify
	if proxy.UpBps > 0 {
		hyConfig.BandwidthConfig.MaxTx = uint64(proxy.UpBps)
	}
	if proxy.DownBps > 0 {
		hyConfig.BandwidthConfig.MaxRx = uint64(proxy.DownBps)
	}

	hyc, err := client.NewReconnectableClient(
		func() (*client.Config, error) { return hyConfig, nil },
		func(c client.Client, info *client.HandshakeInfo, count int) {
			// connection established callback
		},
		false,
	)
	if err != nil {
		return nil, err
	}

	p.clients[name] = &refClient{client: hyc, refCnt: 1}
	return hyc, nil
}

func (p *hysteria2Pool) releaseClient(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if refc, ok := p.clients[name]; ok {
		refc.refCnt--
		if refc.refCnt <= 0 {
			delete(p.clients, name)
			refc.client.Close()
		}
	}
}

type udpConnFactory struct{}

func (f *udpConnFactory) New(addr net.Addr) (net.PacketConn, error) {
	return ListenUDP()
}

type hysteria2Conn struct {
	net.Conn
	pool      *hysteria2Pool
	name      string
	closeOnce sync.Once
}

func (c *hysteria2Conn) Close() error {
	c.closeOnce.Do(func() {
		c.pool.releaseClient(c.name)
		c.Conn.Close()
	})
	return nil
}

// socks5UDPConnFactory creates PacketConns through a SOCKS5 UDP ASSOCIATE relay.
// Used by Hysteria2 when the proxy chain includes a SOCKS5 hop.
type socks5UDPConnFactory struct {
	proxy *config.Proxy
}

func (f *socks5UDPConnFactory) New(addr net.Addr) (net.PacketConn, error) {
	return Socks5UDPAssociate(f.proxy)
}

// Hysteria2Dialer establishes connections through a Hysteria2 server.
type Hysteria2Dialer struct {
	BaseDialer
}

func (d *Hysteria2Dialer) Dial(dstAddr string, dstPort int) (net.Conn, error) {
	pool := getHysteria2Pool()
	hyc, err := pool.getClient(d.Proxy.Name, d.Proxy)
	if err != nil {
		return nil, fmt.Errorf("hysteria2: create client fail: %w", err)
	}

	addr := net.JoinHostPort(dstAddr, strconv.Itoa(dstPort))
	conn, err := hyc.TCP(addr)
	if err != nil {
		pool.releaseClient(d.Proxy.Name)
		return nil, fmt.Errorf("hysteria2: connect to %s fail: %w", addr, err)
	}

	return &hysteria2Conn{
		Conn: conn,
		pool: pool,
		name: d.Proxy.Name,
	}, nil
}

// DialPacket establishes a UDP relay through a Hysteria2 server.
func (d *Hysteria2Dialer) DialPacket() (net.PacketConn, error) {
	pool := getHysteria2Pool()
	hyc, err := pool.getClient(d.Proxy.Name, d.Proxy)
	if err != nil {
		return nil, fmt.Errorf("hysteria2-udp: create client fail: %w", err)
	}

	hyUDP, err := hyc.UDP()
	if err != nil {
		pool.releaseClient(d.Proxy.Name)
		return nil, fmt.Errorf("hysteria2-udp: UDP init fail: %w", err)
	}

	util.LogDebug("[HYSTERIA2-CLI] [%s] [%s] UDP ASSOCIATE via %s:%d", d.Proxy.Name, d.ConnIDStr(), d.Proxy.Server, d.Proxy.Port)

	return &hysteria2PacketConn{
		hyConn: hyUDP,
		pool:   pool,
		name:   d.Proxy.Name,
	}, nil
}

// hysteria2PacketConn wraps client.HyUDPConn as net.PacketConn.
type hysteria2PacketConn struct {
	hyConn    client.HyUDPConn
	pool      *hysteria2Pool
	name      string
	closeOnce sync.Once
}

func (c *hysteria2PacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	data, addrStr, err := c.hyConn.Receive()
	if err != nil {
		return 0, nil, err
	}
	n := copy(b, data)
	addr, err := net.ResolveUDPAddr("udp", addrStr)
	if err != nil {
		return n, nil, err
	}
	return n, addr, nil
}

func (c *hysteria2PacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	err := c.hyConn.Send(b, addr.String())
	if err != nil {
		return 0, fmt.Errorf("hysteria2-udp: send fail: %w", err)
	}
	return len(b), nil
}

func (c *hysteria2PacketConn) Close() error {
	c.closeOnce.Do(func() {
		c.pool.releaseClient(c.name)
		c.hyConn.Close()
	})
	return nil
}

func (c *hysteria2PacketConn) LocalAddr() net.Addr                { return &net.UDPAddr{} }
func (c *hysteria2PacketConn) SetDeadline(t time.Time) error      { return nil }
func (c *hysteria2PacketConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *hysteria2PacketConn) SetWriteDeadline(t time.Time) error { return nil }
