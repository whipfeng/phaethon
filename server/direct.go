package server

import (
	"fmt"
	"net"
	"strings"

	"phaethon/config"
	"phaethon/connlog"
	"phaethon/dialer"
	"phaethon/util"
)

// DirectServer listens on a port and forwards all traffic to a fixed dst
type DirectServer struct {
	BaseServer
}

func (s *DirectServer) Serve(listener net.Listener) {
	AcceptLoop(listener, s, "direct")
}

func (s *DirectServer) HandleConn(clientConn net.Conn) {
	defer clientConn.Close()

	dstHost := s.Mapping.DstHost
	dstPort := s.Mapping.DstPort

	req := config.NewConnectRequest(dstHost, dstPort)
	req = s.RuleConf.Resolving(req)

	proxy := s.RuleConf.Match(req, s.Mapping)
	if proxy == nil {
		util.LogInfo("[DIRECT-SVR] [%s] [conn-N/A] all proxies dead, rejecting %s:%d", s.Mapping.Name, dstHost, dstPort)
		connlog.Log("Direct:"+s.Mapping.Name, "TCP", clientConn.RemoteAddr().String(), dstHost, dstPort, "", "fail", fmt.Errorf("all proxies dead"))
		return
	}
	if strings.ToUpper(proxy.Type) == config.ProxyREJECT {
		util.LogInfo("[DIRECT-SVR] [%s] [conn-N/A] rejected %s:%d", s.Mapping.Name, dstHost, dstPort)
		connlog.Log("Direct:"+s.Mapping.Name, "TCP", clientConn.RemoteAddr().String(), dstHost, dstPort, "", "reject", nil)
		return
	}

	connID := util.NextConnID()
	targetConn, err := dialer.ChainDialWithID(proxy, req.DstAddr, req.DstPort, connID)
	if err != nil {
		util.LogInfo("[DIRECT-SVR] [%s] [%s] connect fail %s:%d: %v", s.Mapping.Name, connID, req.DstAddr, req.DstPort, err)
		connlog.Log("Direct:"+s.Mapping.Name, "TCP", clientConn.RemoteAddr().String(), dstHost, dstPort, proxy.Name, "fail", err)
		return
	}
	defer targetConn.Close()

	util.LogInfo("[DIRECT-SVR] [%s] [%s] %s -> %s:%d via %s(%s)", s.Mapping.Name, connID, clientConn.RemoteAddr(), req.DstAddr, req.DstPort, proxy.Name, proxy.Type)
	connlog.Log("Direct:"+s.Mapping.Name, "TCP", clientConn.RemoteAddr().String(), dstHost, dstPort, proxy.Name, "ok", nil)
	util.RelayWithRateLimit(clientConn, targetConn, proxy.UpRateLimiter, proxy.DownRateLimiter)
}

func StartDirect(ruleConf *config.RuleConfiguration, mapping *config.Mapping) (net.Listener, error) {
	srv := &DirectServer{BaseServer: BaseServer{RuleConf: ruleConf, Mapping: mapping}}
	return startTCP(mapping.Port, srv)
}
