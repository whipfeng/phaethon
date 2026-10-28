package handler

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"phaethon/config"
	"phaethon/connlog"
	"phaethon/db"
	"phaethon/dialer"
	"phaethon/util"

	"phaethon/mesh"
)

// AdminHandler can serve a single network connection directly.
// Implemented by admin.AdminServer to allow bypassing the OS network stack.
type AdminHandler interface {
	ServeConn(conn net.Conn)
}

// ConnectionHandler manages TCP and UDP connection handling for the mesh network.
// It is responsible for rule matching, proxy dialing, and connection relaying.
// This separates connection handling logic from TUN device I/O (which remains in tun.Manager).
type ConnectionHandler struct {
	// Fake-IP pool for domain restoration
	fakeIP *mesh.FakeIPPool

	// Mesh node identity
	localMeshNodeID string
	localMeshVIPs   map[string]bool

	// Admin handler for local node domains
	adminHandler AdminHandler

	// Mode B table for proxy entry connection tracking
	modeBTable *mesh.ModeBTable

	// TUN config for DNS resolution
	tunConfig *config.TUNConfig

	// Callback functions
	isMeshIP         func(ip net.IP) bool
	resolveForDirect func(domain string) ([]net.IP, error)
}

// ConnectionHandlerConfig holds the configuration for creating a ConnectionHandler.
type ConnectionHandlerConfig struct {
	FakeIP          *mesh.FakeIPPool
	LocalMeshNodeID string
	LocalMeshVIPs   map[string]bool
	AdminHandler    AdminHandler
	ModeBTable       *mesh.ModeBTable
	TUNConfig       *config.TUNConfig
	IsMeshIP        func(ip net.IP) bool
	ResolveForDirect func(domain string) ([]net.IP, error)
}

// NewConnectionHandler creates a new ConnectionHandler with the given configuration.
func NewConnectionHandler(cfg ConnectionHandlerConfig) *ConnectionHandler {
	return &ConnectionHandler{
		fakeIP:           cfg.FakeIP,
		localMeshNodeID:  cfg.LocalMeshNodeID,
		localMeshVIPs:    cfg.LocalMeshVIPs,
		adminHandler:     cfg.AdminHandler,
		modeBTable:       cfg.ModeBTable,
		tunConfig:        cfg.TUNConfig,
		isMeshIP:         cfg.IsMeshIP,
		resolveForDirect: cfg.ResolveForDirect,
	}
}

// SetAdminHandler updates the admin handler for local node domain connections.
func (h *ConnectionHandler) SetAdminHandler(handler AdminHandler) {
	h.adminHandler = handler
}

// HandleTCPConn handles an incoming TCP connection from the netstack forwarder.
// It performs rule matching, proxy dialing, and relays data between the connection and the target.
func (h *ConnectionHandler) HandleTCPConn(conn net.Conn, srcAddr string, srcPort int, dstAddr string, dstPort int, inbound string, modeBMapping *config.Mapping) {
	if inbound == "" {
		inbound = "TUN"
	}
	// Compute correlation ID for trace linking
	corrID := fmt.Sprintf("%s:%d->%s:%d", srcAddr, srcPort, dstAddr, dstPort)
	util.LogModuleInfo("handler", "[HANDLECONN-ENTRY] [%s] handleConn START - inbound=%s modeB=%v", corrID, inbound, modeBMapping != nil)
	util.LogModuleDebug("handler", "[TCP-DEBUG] [%s] handleConn called", corrID)
	defer func() {
		util.LogModuleInfo("handler", "[HANDLECONN-EXIT] [%s] handleConn END", corrID)
		conn.Close()
	}()

	// Check if this is a Fake-IP: restore original domain.
	var domain string
	if h.fakeIP != nil {
		if d := h.fakeIP.LookupDomain(dstAddr); d != "" {
			domain = d
			util.LogModuleDebug("handler", "[TCP-DEBUG] [%s] fake-ip lookup: %s -> %s", corrID, dstAddr, domain)
		} else {
			util.LogModuleDebug("handler", "[TCP-DEBUG] [%s] fake-ip lookup: %s -> (no domain)", corrID, dstAddr)
			// Add diagnostic: dump all fake-IP mappings
			if h.fakeIP != nil {
				stats := h.fakeIP.Stats()
				util.LogModuleInfo("handler", "[TCP-DEBUG] [%s] fake-ip pool stats: domainCount=%d realIPCacheCount=%d", 
					corrID, stats.DomainCount, stats.RealIPCacheCount)
			}
		}
	} else {
		util.LogModuleDebug("handler", "[TCP-DEBUG] [%s] fakeIP is nil", corrID)
	}

	// Check if this is a local mesh nodeID domain (nodeID.phn → 127.0.0.1)
	var localNodeDomain bool
	util.LogModuleInfo("handler", "[TCP-DEBUG] [%s] localNodeDomain check: domain=%q localMeshNodeID=%q", corrID, domain, h.localMeshNodeID)
	if domain != "" && h.localMeshNodeID != "" {
		expectedDomain := h.localMeshNodeID + ".phn"
		if domain == expectedDomain {
			localNodeDomain = true
			util.LogModuleInfo("handler", "[TCP-DEBUG] [%s] local mesh nodeID domain MATCH: %s == %s -> will serve via admin handler", corrID, domain, expectedDomain)
		} else {
			util.LogModuleDebug("handler", "[TCP-DEBUG] [%s] domain %s != expected %s", corrID, domain, expectedDomain)
		}
	} else {
		util.LogModuleDebug("handler", "[TCP-DEBUG] [%s] localNodeDomain check skipped: domain=%q localMeshNodeID=%q", corrID, domain, h.localMeshNodeID)
	}

	// Phaethon patch #13: Check if dstAddr is one of our own mesh VIPs.
	// When a connection arrives via mesh P2P to our own VIP (e.g., 100.179.0.10),
	// domain is empty (not a fake-IP), so the nodeID.phn check above doesn't fire.
	// Serve it via admin handler directly.
	if !localNodeDomain && h.localMeshVIPs != nil {
		if h.localMeshVIPs[dstAddr] {
			localNodeDomain = true
			util.LogModuleDebug("handler", "[TCP-DEBUG] [%s] dstAddr %s is local mesh VIP -> will serve via admin handler", corrID, dstAddr)
		}
	}

	// Check if dstAddr is in local mesh subnet (for mesh domain access)
	// When accessing qg.phn, DNS returns an IP in QG's subnet (e.g., 100.0.0.12),
	// which is not a fake-IP and not a VIP, but should still be handled locally.
	if !localNodeDomain && h.isMeshIP != nil {
		if dstIP := net.ParseIP(dstAddr); dstIP != nil && h.isMeshIP(dstIP) {
			localNodeDomain = true
			util.LogModuleInfo("handler", "[HANDLECONN-LOCAL-SUBNET] [%s] dstAddr %s is in local mesh subnet -> will serve via admin handler", corrID, dstAddr)
		}
	}

	connID := util.NextConnID()

	// A dst inside the fake-IP alloc range with no mapping means the client is
	// using a stale fake-IP (pool was reset, e.g., node restart). Close right
	// away instead of direct-dialing a dead address for ~20s; the client's
	// retry re-resolves DNS and gets the fresh fake-IP.
	if domain == "" && h.fakeIP != nil {
		if ip := net.ParseIP(dstAddr); ip != nil && h.fakeIP.InAllocRange(ip) {
			util.LogModuleWarn("handler", "[MESH] [%s] [%s] stale fake-IP dst=%s:%d src=%s rejected (no pool mapping, node restarted?)",
				corrID, connID, dstAddr, dstPort, srcAddr)
			return
		}
	}

	// Diagnostic: log Mode B connections prominently
	if modeBMapping != nil {
		util.LogModuleInfo("handler", "[TCP-DIAG] [%s] [%s] Mode B conn: src=%s dst=%s:%d mapping=%s inbound=%s",
			corrID, connID, srcAddr, dstAddr, dstPort, modeBMapping.Name, inbound)
	}

	// EARLY RETURN: If this is a local mesh node domain, serve via admin handler immediately.
	// Skip rule matching entirely - local mesh domains should NEVER be proxied.
	if localNodeDomain {
		util.LogModuleInfo("handler", "[HANDLECONN-LOCAL] [%s] ENTER localNodeDomain branch (early return)", connID)
		// Direct admin handler: bypass OS network stack entirely
		if h.adminHandler != nil {
			util.LogModuleInfo("handler", "[TCP-DEBUG] [%s] [%s] localNodeDomain=true, serving via admin handler directly (bypassing OS network stack)", corrID, connID)
			util.LogModuleInfo("handler", "[HANDLECONN-LOCAL-ADMIN] [%s] serving via admin handler", connID)
			rec := connlog.Start(inbound, "TCP", srcAddr, domain)
			rec.SetDst("localhost").Establish(connID, nil, false)
			h.adminHandler.ServeConn(conn)
			rec.Close()
			return
		}
		// Fallback: dial localhost via OS network stack
		localIP := dialer.GetLocalIPForDial(nil)
		dialAddr := localIP.String()
		util.LogModuleInfo("handler", "[TCP-DEBUG] [%s] [%s] localNodeDomain=true, dialing local %s:%d (bypassing proxy)", corrID, connID, dialAddr, dstPort)
		targetConn, err := dialer.DialRouteAware("tcp", net.JoinHostPort(dialAddr, strconv.Itoa(dstPort)))
		if err != nil {
			util.LogModuleWarn("handler", "[%s] [%s] [%s] local dial %s:%d fail: %v", corrID, inbound, connID, dialAddr, dstPort, err)
			util.LogModuleInfo("handler", "[HANDLECONN-LOCAL-FAIL] [%s] local dial FAILED: %v", connID, err)
			return
		}
		util.LogModuleInfo("handler", "[HANDLECONN-LOCAL-SUCCESS] [%s] local dial SUCCESS", connID)
		defer targetConn.Close()
		// Start relay
		util.LogModuleInfo("handler", "[TCP-DEBUG] [%s] relay started: %s:%d -> localhost:%d", connID, srcAddr, srcPort, dstPort)
		relay(conn, targetConn)
		return
	}

	// Use domain for rule matching (so domain-based rules work).
	matchAddr := dstAddr
	if domain != "" {
		matchAddr = domain
	}

	req := config.NewConnectRequest("tcp", matchAddr, dstPort)
	var proxy *config.Proxy
	var matchResult *config.MatchResult
	matchMapping := TUNMapping
	if modeBMapping != nil {
		matchMapping = modeBMapping
	}
	var err error
	util.LogModuleInfo("handler", "[HANDLECONN-RESOLVE] [%s] calling db.ResolveMatch for %s:%d mapping=%s", corrID, matchAddr, dstPort, matchMapping.Name)
	req, proxy, matchResult, err = db.ResolveMatch(req, matchMapping.Name)
	if err != nil {
		util.LogModuleWarn("handler", "[%s] tcp resolve match failed: %v", corrID, err)
		return
	}
	util.LogModuleInfo("handler", "[HANDLECONN-RESOLVED] [%s] ResolveMatch done: proxy=%v matchResult=%v", corrID, proxy != nil, matchResult != nil)
	if proxy != nil {
		util.LogModuleInfo("handler", "[HANDLECONN-PROXY] [%s] rule match: %s:%d -> proxy=%s type=%s v2=%v", corrID, matchAddr, dstPort, proxy.Name, proxy.Type, proxy.V2)
		util.LogModuleDebug("handler", "[TCP-DEBUG] [%s] rule match: %s:%d -> proxy=%s type=%s", corrID, matchAddr, dstPort, proxy.Name, proxy.Type)
	} else {
		util.LogModuleInfo("handler", "[HANDLECONN-DIRECT] [%s] rule match: %s:%d -> DIRECT (no proxy matched)", corrID, matchAddr, dstPort)
		util.LogModuleDebug("handler", "[TCP-DEBUG] [%s] rule match: %s:%d -> DIRECT (no proxy matched)", corrID, matchAddr, dstPort)
	}

	// Use the (possibly redirected) destination from Resolving
	resolvedAddr := req.DstAddr
	resolvedPort := req.DstPort
	rec := connlog.Start(inbound, "TCP", srcAddr, matchAddr).Resolve(resolvedAddr, resolvedPort, matchResult)
	defer rec.Close()

	var targetConn net.Conn

	if proxy != nil && strings.ToUpper(proxy.Type) == config.ProxyREJECT {
		util.LogModuleDebug("handler", "[%s] [%s] [%s] %s:%d -> REJECTED", corrID, inbound, connID, resolvedAddr, resolvedPort)
		rec.Reject()
		return
	}

	if localNodeDomain {
		util.LogModuleInfo("handler", "[HANDLECONN-LOCAL] [%s] ENTER localNodeDomain branch", connID)
		// Direct admin handler: bypass OS network stack entirely
		if h.adminHandler != nil {
			util.LogModuleDebug("handler", "[TCP-DEBUG] [%s] [%s] localNodeDomain=true, serving via admin handler directly (bypassing OS network stack)", corrID, connID)
			util.LogModuleInfo("handler", "[HANDLECONN-LOCAL-ADMIN] [%s] serving via admin handler", connID)
			rec.SetDst("localhost").Establish(connID, proxy, false)
			h.adminHandler.ServeConn(conn)
			return
		}
		// Fallback: dial localhost via OS network stack
		localIP := dialer.GetLocalIPForDial(nil)
		dialAddr := localIP.String()
		util.LogModuleDebug("handler", "[TCP-DEBUG] [%s] [%s] localNodeDomain=true, dialing local %s:%d (bypassing proxy)", corrID, connID, dialAddr, resolvedPort)
		targetConn, err = dialer.DialRouteAware("tcp", net.JoinHostPort(dialAddr, strconv.Itoa(resolvedPort)))
		if err != nil {
			util.LogModuleWarn("handler", "[%s] [%s] [%s] local dial %s:%d fail: %v", corrID, inbound, connID, dialAddr, resolvedPort, err)
			util.LogModuleInfo("handler", "[HANDLECONN-LOCAL-FAIL] [%s] local dial FAILED: %v", connID, err)
			rec.SetDst(dialAddr).Fail(err)
			return
		}
		util.LogModuleInfo("handler", "[HANDLECONN-LOCAL-SUCCESS] [%s] local dial SUCCESS", connID)
	} else if proxy != nil && strings.ToUpper(proxy.Type) != config.ProxyDIRECT {
		util.LogModuleInfo("handler", "[HANDLECONN-PROXY-DIAL] [%s] ENTER proxy dial branch: proxy=%s type=%s dst=%s:%d v2=%v", connID, proxy.Name, proxy.Type, resolvedAddr, resolvedPort, proxy.V2)
		util.LogInfo("handler", "[TCP-DEBUG] [%s] dialing via proxy %s: %s:%d (v2=%v)", connID, proxy.Name, resolvedAddr, resolvedPort, proxy.V2)
		targetConn, err = dialer.ChainDialWithID(proxy, resolvedAddr, resolvedPort, connID)
		if err != nil {
			util.LogModuleWarn("handler", "[%s] [%s] dial %s:%d via %s fail: %v", inbound, connID, resolvedAddr, resolvedPort, proxy.Name, err)
			util.LogModuleInfo("handler", "[HANDLECONN-PROXY-FAIL] [%s] proxy dial FAILED: %v", connID, err)
			rec.Fail(err)
			return
		}
		util.LogModuleInfo("handler", "[HANDLECONN-PROXY-SUCCESS] [%s] proxy dial SUCCESS", connID)
		util.LogModuleDebug("handler", "[TCP-DEBUG] [%s] proxy dial success", connID)
	} else {
		util.LogModuleInfo("handler", "[HANDLECONN-DIRECT-DIAL] [%s] ENTER direct dial branch: resolvedAddr=%s resolvedPort=%d", connID, resolvedAddr, resolvedPort)
		// Direct dial: resolvedAddr may be the original domain or a resolver-rewritten
		// host (IP or domain). If it is still a domain, resolve it; if it is an IP,
		// dial it directly.
		dialAddr := resolvedAddr
		if net.ParseIP(dialAddr) == nil {
			// Resolve the real IP for DIRECT connections.
			util.LogModuleDebug("handler", "[TCP-DEBUG] [%s] resolving %s for DIRECT dial", connID, dialAddr)
			if h.resolveForDirect == nil {
				util.LogModuleWarn("handler", "[%s] [%s] resolveForDirect callback is nil", corrID, connID)
				rec.Fail(fmt.Errorf("resolveForDirect callback not set"))
				return
			}
			ips, err := h.resolveForDirect(dialAddr)
			if err != nil || len(ips) == 0 {
				util.LogModuleWarn("handler", "[%s] [%s] resolve %s fail: %v", inbound, connID, dialAddr, err)
				rec.Fail(err)
				return
			}
			// Prefer IPv4
			for _, ip := range ips {
				if ip4 := ip.To4(); ip4 != nil {
					dialAddr = ip4.String()
					break
				}
			}
			util.LogModuleDebug("handler", "[TCP-DEBUG] [%s] resolved %s -> %s", connID, resolvedAddr, dialAddr)
		}
		// Phaethon patch #13: Mesh IP destinations use mesh routing (MeshDial)
		// instead of OS network stack direct dial. Mesh IPs (100.64.0.0/10) are
		// not routable via the OS network stack; they must go through the mesh
		// network's gVisor netstack.
		if ip := net.ParseIP(dialAddr); ip != nil && h.isMeshIP != nil && h.isMeshIP(ip) {
			util.LogModuleDebug("handler", "[TCP-DEBUG] [%s] mesh IP detected: %s, using MeshDial", connID, dialAddr)
			targetConn, err = dialer.MeshDial(dialAddr, resolvedPort, srcAddr, inbound, modeBMapping)
			if err != nil {
				util.LogModuleWarn("handler", "[%s] [%s] mesh dial %s:%d fail: %v", inbound, connID, dialAddr, resolvedPort, err)
				rec.SetDst(dialAddr).Fail(err)
				return
			}
			util.LogModuleDebug("handler", "[TCP-DEBUG] [%s] mesh dial success, starting relay", connID)
		} else {
			util.LogModuleDebug("handler", "[TCP-DEBUG] [%s] dialing tcp %s:%d", connID, dialAddr, resolvedPort)
			targetConn, err = dialer.DialRouteAware("tcp", net.JoinHostPort(dialAddr, fmt.Sprintf("%d", resolvedPort)))
			if err != nil {
				util.LogModuleWarn("handler", "[%s] [%s] direct dial %s:%d fail: %v", inbound, connID, dialAddr, resolvedPort, err)
				rec.SetDst(dialAddr).Fail(err)
				return
			}
			util.LogModuleDebug("handler", "[TCP-DEBUG] [%s] direct dial success, starting relay", connID)
		}
	}
	defer targetConn.Close()

	util.LogModuleDebug("handler", "[TCP-DEBUG] [%s] relay started: %s:%d -> %s", connID, resolvedAddr, resolvedPort, proxyDesc(proxy))
	rec.Establish(connID, proxy, proxy.IsDirect())
	relay(conn, targetConn)
}

// HandleUDPConn handles an incoming UDP connection from the netstack forwarder.
// It performs rule matching, proxy dialing, and relays datagrams between the connection and the target.
func (h *ConnectionHandler) HandleUDPConn(netstackConn net.Conn, srcAddr string, dstAddr string, dstPort int, inbound string, modeBMapping *config.Mapping) {
	if inbound == "" {
		inbound = "TUN"
	}

	// Check if this is a Fake-IP: restore original domain.
	var domain string
	if h.fakeIP != nil {
		if d := h.fakeIP.LookupDomain(dstAddr); d != "" {
			domain = d
			util.LogModuleDebug("handler", "mesh: udp fake-ip %s -> %s", dstAddr, domain)
		}
	}

	// Check if this is a local mesh nodeID domain (nodeID.phn → 127.0.0.1)
	var localNodeDomain bool
	if domain != "" && h.localMeshNodeID != "" {
		expectedDomain := h.localMeshNodeID + ".phn"
		if domain == expectedDomain {
			localNodeDomain = true
			util.LogModuleDebug("handler", "mesh: local mesh nodeID domain detected (udp): %s -> 127.0.0.1", domain)
		}
	}

	connID := util.NextConnID()

	// Use domain for rule matching (so domain-based rules work).
	matchAddr := dstAddr
	if domain != "" {
		matchAddr = domain
	}

	req := config.NewConnectRequest("udp", matchAddr, dstPort)
	var proxy *config.Proxy
	var matchResult *config.MatchResult
	matchMapping := TUNMapping
	if modeBMapping != nil {
		matchMapping = modeBMapping
	}
	var err error
	req, proxy, matchResult, err = db.ResolveMatch(req, matchMapping.Name)
	if err != nil {
		util.LogModuleWarn("handler", "[%s] udp resolve match failed: %v", inbound, err)
		return
	}

	resolvedAddr := req.DstAddr
	resolvedPort := req.DstPort
	rec := connlog.Start(inbound, "UDP", srcAddr, matchAddr).Resolve(resolvedAddr, resolvedPort, matchResult)
	defer rec.Close()

	if proxy != nil && strings.ToUpper(proxy.Type) == config.ProxyREJECT {
		util.LogModuleDebug("handler", "[%s] [%s] udp %s:%d -> REJECTED", inbound, connID, resolvedAddr, resolvedPort)
		rec.Reject()
		return
	}

	var targetConn net.PacketConn
	var dialIP net.IP

	if proxy != nil && strings.ToUpper(proxy.Type) != config.ProxyDIRECT {
		targetConn, err = dialer.ChainUDPDial(proxy)
		if err != nil {
			util.LogModuleWarn("handler", "[%s] [%s] udp dial %s:%d via %s fail: %v", inbound, connID, resolvedAddr, resolvedPort, proxy.Name, err)
			rec.Fail(err)
			return
		}
		dialIP = net.ParseIP(resolvedAddr)
	} else {
		// Direct dial: resolvedAddr may be the original domain or a resolver-rewritten
		// host (IP or domain). If it is still a domain, resolve it; if it is an IP,
		// dial it directly.
		dialIP = net.ParseIP(resolvedAddr)
		if localNodeDomain {
			// Local mesh nodeID domain: connect to localhost
			dialIP = net.ParseIP("127.0.0.1")
			util.LogModuleDebug("handler", "[%s] [%s] udp local mesh nodeID domain: %s -> %s", inbound, connID, domain, dialIP)
		} else if dialIP == nil {
			// Resolve the real IP for DIRECT connections.
			if h.resolveForDirect == nil {
				util.LogModuleWarn("handler", "[%s] [%s] resolveForDirect callback is nil", inbound, connID)
				rec.Fail(fmt.Errorf("resolveForDirect callback not set"))
				return
			}
			ips, err := h.resolveForDirect(resolvedAddr)
			if err != nil || len(ips) == 0 {
				util.LogModuleWarn("handler", "[%s] [%s] udp resolve %s fail: %v", inbound, connID, resolvedAddr, err)
				rec.Fail(err)
				return
			}
			// Prefer IPv4
			for _, ip := range ips {
				if ip4 := ip.To4(); ip4 != nil {
					dialIP = ip4
					break
				}
			}
			util.LogModuleDebug("handler", "[%s] [%s] udp resolved %s -> %s for DIRECT", inbound, connID, resolvedAddr, dialIP)
		}
		targetConn, err = dialer.ListenPacketBoundTo("udp", "", dialIP)
		if err != nil {
			util.LogModuleWarn("handler", "[%s] [%s] udp direct dial %s:%d fail: %v", inbound, connID, resolvedAddr, resolvedPort, err)
			rec.SetDst(dialIP.String()).Fail(err)
			return
		}
	}
	defer targetConn.Close()

	// Use the resolved IP for the destination address.
	dstUDPAddr := &net.UDPAddr{IP: dialIP, Port: resolvedPort}
	util.LogModuleDebug("handler", "[%s] [%s] udp %s:%d -> %s", inbound, connID, resolvedAddr, resolvedPort, proxyDesc(proxy))
	rec.Establish(connID, proxy, proxy.IsDirect())

	relayUDP(netstackConn, targetConn, dstUDPAddr)
}

// TUNMapping is a special mapping that represents traffic entering through
// the TUN interface. Rules can use "#TUN" suffix to target TUN traffic.
var TUNMapping = &config.Mapping{Name: "TUN", Type: "tun"}

// proxyDesc returns a human-readable description of the proxy for logging.
func proxyDesc(p *config.Proxy) string {
	if p == nil || strings.EqualFold(p.Type, config.ProxyDIRECT) {
		return "DIRECT"
	}
	return p.Name
}

// relay copies data bidirectionally between two TCP connections with idle timeout.
func relay(conn, target net.Conn) {
	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())

	done := make(chan struct{})
	defer close(done)

	// Watchdog: close connection if idle for 5 minutes. This reclaims resources
	// from abandoned connections (e.g., NAT drops mapping, client crashes without
	// sending FIN). Applications must send data within 5 minutes to keep alive.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if time.Since(time.Unix(0, lastActivity.Load())) > 5*time.Minute {
					util.LogModuleInfo("handler", "[MESH] relay idle timeout: connection idle for 5 minutes, closing")
					conn.Close()
					target.Close()
					return
				}
			}
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			lastActivity.Store(time.Now().UnixNano())
			n, err := conn.Read(buf)
			if n > 0 {
				lastActivity.Store(time.Now().UnixNano())
				if _, werr := target.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				if cw, ok := target.(interface{ CloseWrite() error }); ok {
					cw.CloseWrite()
				}
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			lastActivity.Store(time.Now().UnixNano())
			n, err := target.Read(buf)
			if n > 0 {
				lastActivity.Store(time.Now().UnixNano())
				if _, werr := conn.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				if cw, ok := conn.(interface{ CloseWrite() error }); ok {
					cw.CloseWrite()
				}
				return
			}
		}
	}()

	wg.Wait()
}

// relayUDP copies datagrams between the netstack UDP connection and the target
// PacketConn, preserving datagram boundaries. Uses a watchdog goroutine to
// enforce a 30-second idle timeout, preventing goroutine leaks when the remote
// stops responding.
func relayUDP(netstackConn net.Conn, targetConn net.PacketConn, dstAddr *net.UDPAddr) {
	const bufSize = 65535
	buf := make([]byte, bufSize)

	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())

	done := make(chan struct{})
	defer close(done)

	// Watchdog: close connection if idle for 30 seconds
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if time.Since(time.Unix(0, lastActivity.Load())) > 30*time.Second {
					util.LogModuleInfo("handler", "[MESH] relayUDP idle timeout: connection idle for 30 seconds, closing")
					netstackConn.Close()
					targetConn.Close()
					return
				}
			}
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)

	// netstack -> target
	go func() {
		defer wg.Done()
		for {
			n, err := netstackConn.Read(buf)
			if n > 0 {
				lastActivity.Store(time.Now().UnixNano())
				if _, err := targetConn.WriteTo(buf[:n], dstAddr); err != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// target -> netstack
	go func() {
		defer wg.Done()
		for {
			n, _, err := targetConn.ReadFrom(buf)
			if n > 0 {
				lastActivity.Store(time.Now().UnixNano())
				if _, err := netstackConn.Write(buf[:n]); err != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	wg.Wait()
}
