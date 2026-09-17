package server

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"phaethon/config"
	"phaethon/connlog"
	"phaethon/dialer"
	"phaethon/util"
)

// HttpProxyServer handles HTTP/HTTPS proxy (CONNECT method and plain HTTP forwarding)
type HttpProxyServer struct {
	BaseServer
}

func (s *HttpProxyServer) Serve(listener net.Listener) {
	AcceptLoop(listener, s, "http")
}

func (s *HttpProxyServer) HandleConn(clientConn net.Conn) {
	defer clientConn.Close()

	// Prevent a stalled client from holding a goroutine forever.
	if ds, ok := clientConn.(interface{ SetReadDeadline(time.Time) error }); ok {
		ds.SetReadDeadline(time.Now().Add(30 * time.Second))
	}

	br := bufio.NewReader(clientConn)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}

	// Request parsed — clear deadline so relay idle timeout takes over.
	if ds, ok := clientConn.(interface{ SetReadDeadline(time.Time) error }); ok {
		ds.SetReadDeadline(time.Time{})
	}

	if req.Method == "CONNECT" {
		s.handleConnect(clientConn, req)
	} else {
		s.handleHTTP(clientConn, br, req)
	}
}

func (s *HttpProxyServer) handleConnect(clientConn net.Conn, req *http.Request) {
	host, port := parseHostPort(req.Host, 443)

	connID := util.NextConnID()
	util.LogInfo("[HTTP-CONNECT] [%s] [%s] %s -> %s:%d mesh dial connecting", s.Mapping.Name, connID, clientConn.RemoteAddr(), host, port)

	targetConn, err := dialer.MeshDial(host, port, clientConn.RemoteAddr().String(), "HTTP:"+s.Mapping.Name)
	if err != nil {
		util.LogInfo("[HTTP-CONNECT] [%s] [%s] connect fail %s:%d: %v", s.Mapping.Name, connID, host, port, err)
		clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer targetConn.Close()

	// Unregister Mode B mapping when connection closes
	if dialer.GlobalModeBTable != nil {
		defer dialer.GlobalModeBTable.Unregister(6, targetConn.LocalAddr())
	}

	clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	connlog.Log("HTTP:"+s.Mapping.Name, "TCP", clientConn.RemoteAddr().String(), host, host, port, &config.MatchResult{ProxyName: "MESH"}, "ok", nil)
	connlog.TrackActive(connID, "HTTP:"+s.Mapping.Name, "TCP", clientConn.RemoteAddr().String(), host, host, port, &config.MatchResult{ProxyName: "MESH"})
	defer connlog.RemoveActive(connID)

	util.LogInfo("[HTTP-CONNECT] [%s] [%s] %s -> %s:%d via MESH", s.Mapping.Name, connID, clientConn.RemoteAddr(), host, port)
	util.RelayWithRateLimit(clientConn, targetConn, nil, nil)
}

func (s *HttpProxyServer) handleHTTP(clientConn net.Conn, br *bufio.Reader, req *http.Request) {
	host, port := parseHostPort(req.Host, 80)

	connID := util.NextConnID()
	util.LogInfo("[HTTP-FWD] [%s] [%s] %s -> %s:%d mesh dial connecting", s.Mapping.Name, connID, clientConn.RemoteAddr(), host, port)

	targetConn, err := dialer.MeshDial(host, port, clientConn.RemoteAddr().String(), "HTTP:"+s.Mapping.Name)
	if err != nil {
		util.LogInfo("[HTTP-FWD] [%s] [%s] forward fail %s:%d: %v", s.Mapping.Name, connID, host, port, err)
		clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer targetConn.Close()

	// Unregister Mode B mapping when connection closes
	if dialer.GlobalModeBTable != nil {
		defer dialer.GlobalModeBTable.Unregister(6, targetConn.LocalAddr())
	}

	// Clean hop-by-hop headers
	cleanHopByHop(req.Header)

	// Adjust URI to relative path
	if req.URL.Host != "" {
		req.URL.Host = ""
		req.URL.Scheme = ""
	}

	// Forward the request
	if err := req.Write(targetConn); err != nil {
		return
	}

	util.LogInfo("[HTTP-FWD] [%s] [%s] %s -> %s:%d via MESH", s.Mapping.Name, connID, clientConn.RemoteAddr(), host, port)
	connlog.Log("HTTP:"+s.Mapping.Name, "TCP", clientConn.RemoteAddr().String(), host, host, port, &config.MatchResult{ProxyName: "MESH"}, "ok", nil)
	connlog.TrackActive(connID, "HTTP:"+s.Mapping.Name, "TCP", clientConn.RemoteAddr().String(), host, host, port, &config.MatchResult{ProxyName: "MESH"})
	defer connlog.RemoveActive(connID)

	// Read response and forward back
	resp, err := http.ReadResponse(bufio.NewReader(targetConn), req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	cleanHopByHop(resp.Header)

	if err := resp.Write(clientConn); err != nil {
		return
	}
}

var hopByHopHeaders = []string{
	"Connection", "Proxy-Authenticate", "Proxy-Authorization", "Proxy-Connection",
}

func cleanHopByHop(h http.Header) {
	connHeader := h.Get("Connection")
	if connHeader != "" {
		for _, token := range strings.Split(connHeader, ",") {
			token = strings.TrimSpace(token)
			if token != "" {
				h.Del(token)
			}
		}
	}
	for _, name := range hopByHopHeaders {
		h.Del(name)
	}
}

func parseHostPort(hostPort string, defaultPort int) (string, int) {
	host, portStr, err := net.SplitHostPort(hostPort)
	if err != nil {
		return hostPort, defaultPort
	}
	port := defaultPort
	fmt.Sscanf(portStr, "%d", &port)
	return host, port
}

func StartHTTP(ruleConf *config.RuleConfiguration, mapping *config.Mapping) (net.Listener, error) {
	srv := &HttpProxyServer{BaseServer: BaseServer{RuleConf: ruleConf, Mapping: mapping}}
	return startTCP(mapping.Port, srv)
}

func StartHTTPS(ruleConf *config.RuleConfiguration, mapping *config.Mapping) (net.Listener, error) {
	srv := &HttpProxyServer{BaseServer: BaseServer{RuleConf: ruleConf, Mapping: mapping}}
	return startTLS(mapping.Port, srv)
}
