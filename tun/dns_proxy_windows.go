//go:build windows

package tun

import (
	"fmt"
	"net"
	"sync"
	"time"

	"phaethon/util"
)

// DNSProxy listens on the TUN adapter IP (UDP:53) and forwards DNS queries into
// the gVisor netstack DNS hijacker. This avoids relying on Wintun to deliver
// unicast packets destined to an IP that is not registered on the adapter.
type DNSProxy struct {
	engine   *Engine
	listener *net.UDPConn
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

// NewDNSProxy creates a proxy for the given engine. Start must be called.
func NewDNSProxy(e *Engine) *DNSProxy {
	return &DNSProxy{
		engine: e,
		stopCh: make(chan struct{}),
	}
}

// Start binds a UDP socket on the TUN adapter IP port 53 and begins forwarding.
func (p *DNSProxy) Start() error {
	hostIP := net.ParseIP("192.0.2.2").To4()
	addr := &net.UDPAddr{IP: hostIP, Port: 53}

	// netsh configures the adapter IP asynchronously; retry binding briefly.
	var conn *net.UDPConn
	var err error
	for i := 0; i < 30; i++ {
		conn, err = net.ListenUDP("udp", addr)
		if err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("listen DNS proxy on %s: %w", addr, err)
	}
	p.listener = conn

	p.wg.Add(1)
	go p.serveLoop()
	util.LogInfo("tun: DNS proxy listening on %s", addr)
	return nil
}

// Stop closes the listener and waits for the serve goroutine to finish.
func (p *DNSProxy) Stop() {
	close(p.stopCh)
	if p.listener != nil {
		p.listener.Close()
	}
	p.wg.Wait()
}

func (p *DNSProxy) serveLoop() {
	defer p.wg.Done()
	buf := make([]byte, 512)
	for {
		select {
		case <-p.stopCh:
			return
		default:
		}

		p.listener.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		n, clientAddr, err := p.listener.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			select {
			case <-p.stopCh:
				return
			default:
				util.LogWarn("tun dns proxy: read error: %v", err)
				continue
			}
		}

		query := make([]byte, n)
		copy(query, buf[:n])
		go p.handleQuery(query, clientAddr)
	}
}

func (p *DNSProxy) handleQuery(query []byte, clientAddr *net.UDPAddr) {
	resp, err := p.engine.queryInternalDNS(query)
	if err != nil {
		util.LogWarn("tun dns proxy: internal query failed: %v", err)
		return
	}

	if _, err := p.listener.WriteToUDP(resp, clientAddr); err != nil {
		util.LogWarn("tun dns proxy: write response to %s failed: %v", clientAddr, err)
	}
}
