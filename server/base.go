package server

import (
	"crypto/tls"
	"fmt"
	"net"
	"time"

	"phaethon/config"
	"phaethon/util"
)

// BaseServer holds fields common to all protocol servers.
type BaseServer struct {
	RuleConf *config.RuleConfiguration
	Mapping  *config.Mapping
}

// ConnHandler is the interface for protocol-specific connection handlers.
type ConnHandler interface {
	HandleConn(net.Conn)
}

// Server is the full interface for protocol servers.
type Server interface {
	ConnHandler
	Serve(net.Listener)
}

// configureTCPConn enables TCP keep-alive (30s probe) and disables Nagle's
// algorithm so small interactive packets (handshakes, control frames,
// heartbeats) are sent immediately without ~40ms stalls.
func configureTCPConn(conn net.Conn) {
	// Unwrap TLS to reach the underlying TCP socket.
	if tlsConn, ok := conn.(*tls.Conn); ok {
		if nc := tlsConn.NetConn(); nc != nil {
			conn = nc
		}
	}

	type tcpSetter interface {
		SetKeepAlive(bool) error
		SetKeepAlivePeriod(time.Duration) error
		SetNoDelay(bool) error
	}
	if tc, ok := conn.(tcpSetter); ok {
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(30 * time.Second)
		tc.SetNoDelay(true)
	}
}

// AcceptLoop runs the standard accept loop for any ConnHandler.
func AcceptLoop(listener net.Listener, handler ConnHandler, name string) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				util.LogWarn("%s accept temporary error: %v", name, err)
				time.Sleep(5 * time.Millisecond)
				continue
			}
			util.LogError("%s accept error: %v", name, err)
			return
		}
		configureTCPConn(conn)
		go handler.HandleConn(conn)
	}
}

// startTCP creates a TCP listener on the given port and starts serving.
func startTCP(port int, srv Server) (net.Listener, error) {
	addr := fmt.Sprintf(":%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	go srv.Serve(ln)
	return ln, nil
}

// startTLS creates a TLS listener on the given port and starts serving.
func startTLS(port int, srv Server) (net.Listener, error) {
	cert, err := util.GenerateSelfSignedCert()
	if err != nil {
		return nil, err
	}
	addr := fmt.Sprintf(":%d", port)
	ln, err := tls.Listen("tcp", addr, &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		return nil, err
	}
	go srv.Serve(ln)
	return ln, nil
}
