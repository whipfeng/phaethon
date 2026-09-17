package server

import (
	"net"

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

	connID := util.NextConnID()
	util.LogInfo("[DIRECT-SVR] [%s] [%s] %s -> %s:%d mesh dial connecting", s.Mapping.Name, connID, clientConn.RemoteAddr(), dstHost, dstPort)

	targetConn, err := dialer.MeshDial(dstHost, dstPort, clientConn.RemoteAddr().String(), "Direct:"+s.Mapping.Name)
	if err != nil {
		util.LogInfo("[DIRECT-SVR] [%s] [%s] connect fail %s:%d: %v", s.Mapping.Name, connID, dstHost, dstPort, err)
		return
	}
	defer targetConn.Close()

	// Unregister Mode B mapping when connection closes
	if dialer.GlobalModeBTable != nil {
		defer dialer.GlobalModeBTable.Unregister(6, targetConn.LocalAddr())
	}

	util.LogInfo("[DIRECT-SVR] [%s] [%s] %s -> %s:%d via MESH", s.Mapping.Name, connID, clientConn.RemoteAddr(), dstHost, dstPort)
	connlog.Log("Direct:"+s.Mapping.Name, "TCP", clientConn.RemoteAddr().String(), dstHost, dstHost, dstPort, &config.MatchResult{ProxyName: "MESH"}, "ok", nil)
	connlog.TrackActive(connID, "Direct:"+s.Mapping.Name, "TCP", clientConn.RemoteAddr().String(), dstHost, dstHost, dstPort, &config.MatchResult{ProxyName: "MESH"})
	defer connlog.RemoveActive(connID)
	util.RelayWithRateLimit(clientConn, targetConn, nil, nil)
}

func StartDirect(ruleConf *config.RuleConfiguration, mapping *config.Mapping) (net.Listener, error) {
	srv := &DirectServer{BaseServer: BaseServer{RuleConf: ruleConf, Mapping: mapping}}
	return startTCP(mapping.Port, srv)
}
