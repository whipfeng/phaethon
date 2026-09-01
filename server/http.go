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

	addrReq := config.NewConnectRequest(host, port)
	addrReq = s.RuleConf.Resolving(addrReq)

	proxy := s.RuleConf.Match(addrReq, s.Mapping)
	if proxy == nil {
		util.LogInfo("[HTTP-CONNECT] [%s] [conn-N/A] all proxies dead, rejecting %s:%d", s.Mapping.Name, addrReq.DstAddr, addrReq.DstPort)
		connlog.Log("HTTP:"+s.Mapping.Name, "TCP", clientConn.RemoteAddr().String(), addrReq.DstAddr, addrReq.DstPort, "", "fail", fmt.Errorf("all proxies dead"))
		clientConn.Write([]byte("HTTP/1.1 403 Forbidden\r\n\r\n"))
		return
	}
	if strings.ToUpper(proxy.Type) == config.ProxyREJECT {
		util.LogInfo("[HTTP-CONNECT] [%s] [conn-N/A] rejected %s:%d", s.Mapping.Name, addrReq.DstAddr, addrReq.DstPort)
		connlog.Log("HTTP:"+s.Mapping.Name, "TCP", clientConn.RemoteAddr().String(), addrReq.DstAddr, addrReq.DstPort, "", "reject", nil)
		clientConn.Write([]byte("HTTP/1.1 403 Forbidden\r\n\r\n"))
		return
	}

	connID := util.NextConnID()
	targetConn, err := dialer.ChainDialWithID(proxy, addrReq.DstAddr, addrReq.DstPort, connID)
	if err != nil {
		util.LogInfo("[HTTP-CONNECT] [%s] [%s] connect fail %s:%d: %v", s.Mapping.Name, connID, addrReq.DstAddr, addrReq.DstPort, err)
		connlog.Log("HTTP:"+s.Mapping.Name, "TCP", clientConn.RemoteAddr().String(), addrReq.DstAddr, addrReq.DstPort, proxy.Name, "fail", err)
		clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer targetConn.Close()

	clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	connlog.Log("HTTP:"+s.Mapping.Name, "TCP", clientConn.RemoteAddr().String(), addrReq.DstAddr, addrReq.DstPort, proxy.Name, "ok", nil)

	util.LogInfo("[HTTP-CONNECT] [%s] [%s] %s -> %s:%d via %s(%s)", s.Mapping.Name, connID, clientConn.RemoteAddr(), addrReq.DstAddr, addrReq.DstPort, proxy.Name, proxy.Type)
	util.RelayWithRateLimit(clientConn, targetConn, proxy.UpRateLimiter, proxy.DownRateLimiter)
}

func (s *HttpProxyServer) handleHTTP(clientConn net.Conn, br *bufio.Reader, req *http.Request) {
	host, port := parseHostPort(req.Host, 80)

	addrReq := config.NewConnectRequest(host, port)
	addrReq = s.RuleConf.Resolving(addrReq)

	proxy := s.RuleConf.Match(addrReq, s.Mapping)
	if proxy == nil {
		util.LogInfo("[HTTP-FWD] [%s] [conn-N/A] all proxies dead, rejecting %s:%d", s.Mapping.Name, addrReq.DstAddr, addrReq.DstPort)
		connlog.Log("HTTP:"+s.Mapping.Name, "TCP", clientConn.RemoteAddr().String(), addrReq.DstAddr, addrReq.DstPort, "", "fail", fmt.Errorf("all proxies dead"))
		clientConn.Write([]byte("HTTP/1.1 403 Forbidden\r\n\r\n"))
		return
	}
	if strings.ToUpper(proxy.Type) == config.ProxyREJECT {
		util.LogInfo("[HTTP-FWD] [%s] [conn-N/A] rejected %s:%d", s.Mapping.Name, addrReq.DstAddr, addrReq.DstPort)
		connlog.Log("HTTP:"+s.Mapping.Name, "TCP", clientConn.RemoteAddr().String(), addrReq.DstAddr, addrReq.DstPort, "", "reject", nil)
		clientConn.Write([]byte("HTTP/1.1 403 Forbidden\r\n\r\n"))
		return
	}

	connID := util.NextConnID()
	targetConn, err := dialer.ChainDialWithID(proxy, addrReq.DstAddr, addrReq.DstPort, connID)
	if err != nil {
		util.LogInfo("[HTTP-FWD] [%s] [%s] forward fail %s:%d: %v", s.Mapping.Name, connID, addrReq.DstAddr, addrReq.DstPort, err)
		connlog.Log("HTTP:"+s.Mapping.Name, "TCP", clientConn.RemoteAddr().String(), addrReq.DstAddr, addrReq.DstPort, proxy.Name, "fail", err)
		clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer targetConn.Close()

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

	util.LogInfo("[HTTP-FWD] [%s] [%s] %s -> %s:%d via %s(%s)", s.Mapping.Name, connID, clientConn.RemoteAddr(), addrReq.DstAddr, addrReq.DstPort, proxy.Name, proxy.Type)
	connlog.Log("HTTP:"+s.Mapping.Name, "TCP", clientConn.RemoteAddr().String(), addrReq.DstAddr, addrReq.DstPort, proxy.Name, "ok", nil)

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
