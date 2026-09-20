package server

import (
	"net"

	"phaethon/config"
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

	util.LogInfo("[DEBUG] [DIRECT-SVR] HandleConn called from %s", clientConn.RemoteAddr())

	dstHost := s.Mapping.DstHost
	dstPort := s.Mapping.DstPort

	connID := util.NextConnID()
	util.LogInfo("[DIRECT-SVR] [%s] [%s] %s -> %s:%d mesh dial connecting", s.Mapping.Name, connID, clientConn.RemoteAddr(), dstHost, dstPort)

	targetConn, cleanup, err := s.MeshDialWithModeB(dstHost, dstPort, clientConn.RemoteAddr().String(), "Direct")
	if err != nil {
		util.LogInfo("[DIRECT-SVR] [%s] [%s] connect fail %s:%d: %v", s.Mapping.Name, connID, dstHost, dstPort, err)
		return
	}
	defer targetConn.Close()
	defer cleanup()

	util.LogInfo("[DIRECT-SVR] [%s] [%s] %s -> %s:%d via MESH", s.Mapping.Name, connID, clientConn.RemoteAddr(), dstHost, dstPort)
	util.RelayWithRateLimit(clientConn, targetConn, nil, nil)
}

func StartDirect(ruleConf *config.RuleConfiguration, mapping *config.Mapping) (net.Listener, error) {
	srv := &DirectServer{BaseServer: BaseServer{RuleConf: ruleConf, Mapping: mapping}}
	return startTCP(mapping.Port, srv)
}
