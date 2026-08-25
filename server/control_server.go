package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"phaethon/config"
	"phaethon/reverse"
	"phaethon/util"
)

// GlobalControlManager is the singleton control manager instance.
// Set by main() before any listener accepts connections.
var GlobalControlManager *ControlManager

// ControlSession tracks an active reverse-side control connection.
type ControlSession struct {
	Address      string
	RegistryAddr string // registry-side local address this control connection arrived on
	Conn         net.Conn
	stopCh       chan struct{}
	DynAddr      string // populated after register so HandleClose can clean up by dyn address
	ReverseID    string // set after register command, used for binding-store cleanup
	Seq          int    // set after register command, used for binding-store cleanup
	mu           sync.Mutex
	lastActivity time.Time // updated on every received frame
}

// touch marks the session as active right now.
func (s *ControlSession) touch() {
	s.mu.Lock()
	s.lastActivity = time.Now()
	s.mu.Unlock()
}

// isActive reports whether the session has received traffic recently.
func (s *ControlSession) isActive(timeout time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.lastActivity) < timeout
}

// DynamicResource holds all dynamically created resources for one address.
type DynamicResource struct {
	Address     string
	Proto       string
	Port        int
	Listener    net.Listener
	Proxy       *config.Proxy
	MappingName string // used to clean up the scoped MATCH rule on HandleClose
	ReverseID   string // reverse client identity for force-delete lookups
	Seq         int    // stable sequence number for this reverse config
}

// ControlManager manages control connections and their dynamic resources.
type ControlManager struct {
	mu           sync.RWMutex
	sessions     map[string]*ControlSession  // address -> session
	resources    map[string]*DynamicResource // address -> resource
	ruleConf     *config.RuleConfiguration
	portMin      int           // dynamic port range min (0 = unrestricted)
	portMax      int           // dynamic port range max (0 = unrestricted)
	portNext     int           // next port to try within range (round-robin)
	bindingStore *BindingStore // persistent reverse_id -> port binding
	pendingMu    sync.Mutex
	pendingRegs  map[string]bool // reverseID#seq -> registration in flight
}

// parsePortRange parses a "min-max" string into (min, max). Returns (0,0) if empty.
func parsePortRange(s string) (min, max int, err error) {
	if s == "" {
		return 0, 0, nil
	}
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid port range format: %s (expected min-max)", s)
	}
	min, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	max, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || min <= 0 || max <= 0 || min > max {
		return 0, 0, fmt.Errorf("invalid port range: %s", s)
	}
	return min, max, nil
}

// NewControlManager creates a new ControlManager.
func NewControlManager(ruleConf *config.RuleConfiguration, dataDir string) *ControlManager {
	m := &ControlManager{
		sessions:     make(map[string]*ControlSession),
		resources:    make(map[string]*DynamicResource),
		ruleConf:     ruleConf,
		bindingStore: NewBindingStore(dataDir),
		pendingRegs:  make(map[string]bool),
	}
	if ruleConf != nil && ruleConf.TCPPortRange != "" {
		min, max, err := parsePortRange(ruleConf.TCPPortRange)
		if err == nil && min > 0 && max > 0 {
			m.portMin = min
			m.portMax = max
		}
	}
	return m
}

// GetBindings returns a snapshot of the current reverse port bindings.
func (m *ControlManager) GetBindings() []PortBinding {
	if m == nil || m.bindingStore == nil {
		return nil
	}
	return m.bindingStore.Snapshot()
}

// IsDynamicAddress checks if addr was allocated by this manager.
func (m *ControlManager) IsDynamicAddress(addr string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.resources[addr]
	return ok
}

// HandleControlConnection is called when a BIND with PORT=1 arrives.
// It runs the control session loop: heartbeat + JSON command handling.
func (m *ControlManager) HandleControlConnection(conn net.Conn, address string) {
	// Use the remote address as the session key so multiple control connections
	// from the same registry address do not overwrite each other.
	remoteAddr := conn.RemoteAddr().String()
	if remoteAddr == "" {
		remoteAddr = address
	}
	registryAddr := conn.LocalAddr().String()
	if registryAddr == "" {
		registryAddr = address
	}

	session := &ControlSession{
		Address:      remoteAddr,
		RegistryAddr: registryAddr,
		Conn:         conn,
		stopCh:       make(chan struct{}),
		lastActivity: time.Now(),
	}

	m.mu.Lock()
	m.sessions[remoteAddr] = session
	m.mu.Unlock()

	util.LogInfo("[CONTROL] session started for %s (registry %s)", remoteAddr, address)
	defer func() {
		m.HandleClose(remoteAddr)
		util.LogInfo("[CONTROL] session ended for %s", remoteAddr)
	}()

	go m.sendHeartbeats(session)
	m.runSession(session)
}

// sendHeartbeats sends FrameHeartbeat every 30s on the control connection.
func (m *ControlManager) sendHeartbeats(session *ControlSession) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-session.stopCh:
			return
		case <-ticker.C:
			if err := reverse.WriteFrame(session.Conn, reverse.FrameHeartbeat, nil); err != nil {
				return
			}
		}
	}
}

// runSession reads FrameData payloads as JSON commands and dispatches them.
func (m *ControlManager) runSession(session *ControlSession) {
	for {
		session.Conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		frameType, payload, err := reverse.ReadFrame(session.Conn)
		session.touch()
		if err != nil {
			util.LogError("[CONTROL] read error for %s: %v", session.Address, err)
			return
		}

		switch frameType {
		case reverse.FrameHeartbeat:
			continue
		default:
			// Treat non-heartbeat frames as potential JSON commands
			if len(payload) > 0 && frameType == reverse.FrameData {
				reply := m.handleCommand(session, payload)
				if reply.Status == "ok" && reply.Address != "" {
					session.DynAddr = reply.Address
				}
				respBytes, _ := json.Marshal(reply)
				reverse.WriteFrame(session.Conn, reverse.FrameData, respBytes)
			} else if frameType == reverse.FrameHeartbeat {
				continue
			} else {
				util.LogInfo("[CONTROL] unexpected frame type 0x%02x for %s", frameType, session.Address)
			}
		}
	}
}

// handleCommand parses and executes a JSON control command.
func (m *ControlManager) handleCommand(session *ControlSession, payload []byte) reverse.ControlReply {
	var req reverse.ControlRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return reverse.ControlReply{Status: "error", Error: "invalid json"}
	}

	switch req.Cmd {
	case "register":
		return m.handleRegister(session.Address, session.RegistryAddr, &req)
	default:
		return reverse.ControlReply{Status: "error", Error: "unknown command: " + req.Cmd}
	}
}

// closeSessionByIdentity closes a stale control session for the same reverse
// identity (ReverseID + Seq). If the session is still active (recent traffic), it returns true and
// does not close it, so the caller can reject the duplicate registration.
func (m *ControlManager) closeSessionByIdentity(reverseID string, seq int) bool {
	if reverseID == "" {
		return false
	}
	m.mu.RLock()
	var oldAddr string
	var oldSess *ControlSession
	for addr, sess := range m.sessions {
		if sess.ReverseID == reverseID && sess.Seq == seq {
			oldAddr = addr
			oldSess = sess
			break
		}
	}
	m.mu.RUnlock()
	if oldAddr == "" || oldSess == nil {
		return false
	}
	if oldSess.isActive(45 * time.Second) {
		util.LogInfo("[CONTROL] reverse %s#%d already has an active session %s, rejecting duplicate", reverseID, seq, oldAddr)
		return true
	}
	util.LogInfo("[CONTROL] reverse %s#%d re-registering, closing stale session %s", reverseID, seq, oldAddr)
	m.HandleClose(oldAddr)
	return false
}

// handleRegister processes a register request: allocates address, opens listener,
// creates proxy, inserts routing rule.
func (m *ControlManager) handleRegister(controlAddr string, registryAddr string, req *reverse.ControlRequest) reverse.ControlReply {
	proto := req.Proto
	if proto == "" {
		proto = "socks5"
	}
	proto = strings.ToLower(proto)

	listenerProto := req.ListenerProto
	if listenerProto == "" {
		listenerProto = "socks5"
	}
	listenerProto = strings.ToLower(listenerProto)

	// Reject anonymous registrations that provide no identifying information.
	// A well-behaved reverse client must send at least a Name or a ReverseID
	// so the admin panel can display meaningful entries and stale bindings can
	// be properly tracked across reconnects.
	if req.Name == "" && req.ReverseID == "" {
		util.LogWarn("[CONTROL] rejected anonymous registration from %s: no name or reverse_id", controlAddr)
		return reverse.ControlReply{Status: "error", Error: "registration requires name or reverse_id for identification"}
	}

	// Serialize registrations for the same (ReverseID, Seq) identity so that
	// simultaneous reconnects cannot both pass the stale-session check and
	// create duplicate active sessions.
	if req.ReverseID != "" {
		regKey := fmt.Sprintf("%s#%d", req.ReverseID, req.Seq)
		m.pendingMu.Lock()
		if m.pendingRegs[regKey] {
			m.pendingMu.Unlock()
			return reverse.ControlReply{Status: "error", Error: "registration already in progress for this identity"}
		}
		m.pendingRegs[regKey] = true
		m.pendingMu.Unlock()
		defer func() {
			m.pendingMu.Lock()
			delete(m.pendingRegs, regKey)
			m.pendingMu.Unlock()
		}()
	}

	// Use registry-side configured port range
	portMin, portMax := m.portMin, m.portMax

	// Validate direct listener configuration.
	if listenerProto == "direct" && (req.DirectDstHost == "" || req.DirectDstPort == 0) {
		return reverse.ControlReply{Status: "error", Error: "direct listener requires direct-dst-host and direct-dst-port"}
	}

	// If this reverse identity already has an active session, close it so the port can
	// be reused instead of allocating a new one.
	if req.ReverseID != "" && m.closeSessionByIdentity(req.ReverseID, req.Seq) {
		return reverse.ControlReply{Status: "error", Error: "reverse identity already connected from another instance"}
	}

	// Generate unique address
	dynAddr := generateDynAddress()

	// Allocate port: honor a user-selected preferred port first, then fall back
	// to the persistent binding for this ReverseID+Seq, then auto-allocate.
	port := 0
	preferred := req.PreferredPort

	if preferred > 0 {
		if preferred > 65535 {
			return reverse.ControlReply{Status: "error", Error: fmt.Sprintf("preferred port %d out of valid range (1-65535)", preferred)}
		}
		if portMin > 0 && portMax > 0 && (preferred < portMin || preferred > portMax) {
			return reverse.ControlReply{Status: "error", Error: fmt.Sprintf("preferred port %d outside configured range %d-%d", preferred, portMin, portMax)}
		}
		if !m.isPortAvailable(preferred, req.ReverseID, req.Seq) {
			return reverse.ControlReply{Status: "error", Error: fmt.Sprintf("preferred port %d unavailable: %s", preferred, m.portConflictReason(preferred, req.ReverseID, req.Seq))}
		}
		port = preferred
	} else if req.ReverseID != "" {
		if b := m.bindingStore.Get(req.ReverseID, req.Seq); b != nil && b.Port > 0 {
			if (portMin <= 0 || (b.Port >= portMin && b.Port <= portMax)) && m.isPortAvailable(b.Port, req.ReverseID, req.Seq) {
				port = b.Port
			}
		}
	}

	if port == 0 {
		var err error
		port, err = m.allocatePort(portMin, portMax, req.ReverseID, req.Seq)
		if err != nil {
			return reverse.ControlReply{Status: "error", Error: err.Error()}
		}
	}

	// Create dynamic listener with full credentials
	listener, mapping, err := m.createListener(listenerProto, port, dynAddr,
		req.ListenerUser, req.ListenerPassword, req.ListenerSNI,
		req.DirectDstHost, req.DirectDstPort)
	if err != nil {
		// Port may be occupied by an external process. Remove the binding so
		// the next reconnect allocates a fresh port instead of looping on the
		// same occupied one.
		if req.ReverseID != "" {
			m.bindingStore.Remove(req.ReverseID, req.Seq)
		}
		return reverse.ControlReply{Status: "error", Error: fmt.Sprintf("create listener fail: %v", err)}
	}

	// Always record a binding so the admin panel shows all active reverse
	// clients in real-time via SSE.  When the client sends a ReverseID we use
	// it as the stable key (survives reconnects); otherwise we fall back to
	// the dynamic address assigned by the registry.
	bindID := req.ReverseID
	bindSeq := req.Seq
	if bindID == "" {
		bindID = dynAddr
		bindSeq = 0
	}
	identity := fmt.Sprintf("%s#%d", req.Name, req.Seq)
	if identity == "#0" {
		identity = fmt.Sprintf("%s|%s", listenerProto, dynAddr)
	}
	m.bindingStore.Set(bindID, bindSeq, port, listenerProto, identity, req.DirectDstHost, req.DirectDstPort, req.OutboundProxy, controlAddr, registryAddr)

	hexID := dynAddr[4:12] // strip "dyn-" prefix, keep 8 hex chars
	// The dynamic proxy routes external connections back through the reverse
	// tunnel. It must be a real proxy protocol (socks5/trojan/...) so the
	// destination can be carried inside the proxy framing. For a direct
	// listener we still use SOCKS5 as the tunnel-side protocol.
	reverseProto := listenerProto
	if listenerProto == "direct" {
		reverseProto = "socks5"
	}
	proxy := &config.Proxy{
		Name:           "dyn-proxy-" + hexID,
		Type:           reverseProto,
		ReverseAddress: dynAddr,
	}

	m.ruleConf.Lock()
	m.ruleConf.Proxies = append(m.ruleConf.Proxies, proxy)
	m.ruleConf.ProxyNames[proxy.Name] = proxy
	m.ruleConf.Unlock()

	resource := &DynamicResource{
		Address:     dynAddr,
		Proto:       listenerProto,
		Port:        port,
		Listener:    listener,
		Proxy:       proxy,
		MappingName: mapping.Name,
		ReverseID:   req.ReverseID,
		Seq:         req.Seq,
	}

	m.mu.Lock()
	m.resources[dynAddr] = resource
	if sess, ok := m.sessions[controlAddr]; ok {
		sess.ReverseID = req.ReverseID
		sess.Seq = req.Seq
	}
	m.mu.Unlock()

	m.insertRoutingRule(mapping.Name, proxy)

	util.LogInfo("[CONTROL] registered %s: register=%s listener=%s port=%d address=%s",
		controlAddr, proto, listenerProto, port, dynAddr)

	return reverse.ControlReply{
		Status:  "ok",
		Address: dynAddr,
		Port:    port,
	}
}

// allocatePort finds an available port.
//   - No range specified   : asks the OS via net.Listen(":0").
//   - Range specified      : round-robin within [portMin, portMax].
//     If exhausted, retries from portMin; then
//     reclaims the oldest disconnected binding and
//     retries; returns error if still none free.
func (m *ControlManager) allocatePort(portMin, portMax int, reverseID string, seq int) (int, error) {
	// 1. No range restriction — let the OS pick.
	if portMin <= 0 || portMax <= 0 || portMin > portMax {
		m.mu.Lock()
		defer m.mu.Unlock()
		for i := 0; i < 10; i++ {
			ln, err := net.Listen("tcp", ":0")
			if err != nil {
				continue
			}
			p := ln.Addr().(*net.TCPAddr).Port
			ln.Close()
			if m.isPortAvailableLocked(p, reverseID, seq) {
				return p, nil
			}
		}
		return 0, fmt.Errorf("no free port available (unrestricted range)")
	}

	// 2. Range specified — round-robin from last position.
	m.mu.Lock()
	rangeSize := portMax - portMin + 1
	start := portMin
	if m.portNext >= portMin && m.portNext <= portMax {
		start = m.portNext
	}
	for i := 0; i < rangeSize; i++ {
		p := start + i
		if p > portMax {
			p = portMin + (p - portMax - 1)
		}
		if m.isPortAvailableLocked(p, reverseID, seq) {
			m.portNext = p + 1
			if m.portNext > portMax {
				m.portNext = portMin
			}
			m.mu.Unlock()
			return p, nil
		}
	}

	// 3. Exhausted — retry from portMin (a just-disconnected node may have
	// freed its port, and HandleClose will have set DisconnectedAt).
	for p := portMin; p <= portMax; p++ {
		if m.isPortAvailableLocked(p, reverseID, seq) {
			m.portNext = p + 1
			if m.portNext > portMax {
				m.portNext = portMin
			}
			m.mu.Unlock()
			return p, nil
		}
	}
	m.mu.Unlock()

	// 4. Still exhausted — reclaim the oldest disconnected binding.
	oldest := m.bindingStore.FindOldestDisconnectedBinding(portMin, portMax)
	if oldest != nil {
		// Double-check: the reverse identity must not have an active session.
		m.mu.RLock()
		_, hasSession := m.sessions[oldest.ReverseID]
		m.mu.RUnlock()
		if !hasSession {
			util.LogInfo("[CONTROL] reclaiming oldest disconnected binding %s#%d port=%d (disconnected %s ago)",
				oldest.ReverseID, oldest.Seq, oldest.Port, time.Since(oldest.DisconnectedAt).Round(time.Second))
			m.bindingStore.Remove(oldest.ReverseID, oldest.Seq)
		}
	}

	// Retry after reclaiming.
	m.mu.Lock()
	for p := portMin; p <= portMax; p++ {
		if m.isPortAvailableLocked(p, reverseID, seq) {
			m.portNext = p + 1
			if m.portNext > portMax {
				m.portNext = portMin
			}
			m.mu.Unlock()
			return p, nil
		}
	}
	m.mu.Unlock()

	return 0, fmt.Errorf("port range %d-%d exhausted", portMin, portMax)
}

func (m *ControlManager) isPortAvailable(port int, reverseID string, seq int) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.isPortAvailableLocked(port, reverseID, seq)
}

// portConflictReason returns a human-readable explanation of why a port is not
// available, without modifying any state.
func (m *ControlManager) portConflictReason(port int, reverseID string, seq int) string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, r := range m.resources {
		if r.Port == port {
			return "already allocated to an active reverse session"
		}
	}
	for _, mapping := range m.ruleConf.Mappings {
		if !mapping.IsEnabled() {
			continue
		}
		if mapping.Port == port {
			return fmt.Sprintf("already used by static mapping %s", mapping.Name)
		}
	}
	if reverseID != "" && m.bindingStore.IsPortBoundByOther(port, reverseID, seq) {
		if b := m.bindingStore.FindBindingByPort(port); b != nil {
			return fmt.Sprintf("already bound to reverse identity %s#%d", b.ReverseID, b.Seq)
		}
		return "already bound to another reverse identity"
	}
	addr := fmt.Sprintf(":%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "occupied by an external process"
	}
	ln.Close()
	return "already in use"
}

func (m *ControlManager) isPortAvailableLocked(port int, reverseID string, seq int) bool {
	for _, r := range m.resources {
		if r.Port == port {
			return false
		}
	}
	// Also check static mappings in config
	for _, mapping := range m.ruleConf.Mappings {
		if !mapping.IsEnabled() {
			continue
		}
		if mapping.Port == port {
			return false
		}
	}
	// Also check bindingStore: a port bound to another reverse identity should not be
	// reassigned, even if that reverse identity is currently offline.
	if reverseID != "" && m.bindingStore.IsPortBoundByOther(port, reverseID, seq) {
		return false
	}
	// Check OS-level occupation to avoid assigning a port already taken by
	// an external process (which would cause createListener to fail and retry).
	addr := fmt.Sprintf(":%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// createListener starts a protocol listener on the given port with optional credentials.
func (m *ControlManager) createListener(proto string, port int, dynAddr string,
	user, password, sni, directHost string, directPort int) (net.Listener, *config.Mapping, error) {

	mapping := &config.Mapping{
		Name:     "dyn-map-" + dynAddr[4:12],
		Type:     proto,
		Port:     port,
		Username: user,
		Password: password,
		Sni:      sni,
		DstHost:  directHost,
		DstPort:  directPort,
	}

	var ln net.Listener
	var err error

	switch proto {
	case "socks5":
		srv := &Socks5Server{BaseServer: BaseServer{RuleConf: m.ruleConf, Mapping: mapping}}
		ln, err = StartSocks5(m.ruleConf, mapping)
		if err != nil {
			return nil, nil, err
		}
		go srv.Serve(ln)

	case "trojan":
		pw := util.Sha224Hex(password)
		srv := &TrojanServer{
			BaseServer: BaseServer{RuleConf: m.ruleConf, Mapping: mapping},
			Password:   pw,
		}
		ln, err = StartTrojan(m.ruleConf, mapping)
		if err != nil {
			return nil, nil, err
		}
		go srv.Serve(ln)

	case "direct":
		ln, err = StartDirect(m.ruleConf, mapping)
		if err != nil {
			return nil, nil, err
		}
		srv := &DirectServer{BaseServer: BaseServer{RuleConf: m.ruleConf, Mapping: mapping}}
		go srv.Serve(ln)

	default:
		return nil, nil, fmt.Errorf("unsupported listener protocol: %s", proto)
	}

	return ln, mapping, nil
}

// insertRoutingRule adds a MATCH-all rule scoped to the dynamic mapping,
// so all traffic from that listener routes to its dedicated reverse proxy.
// Format: MATCH,proxyName#mappingName — the # suffix scopes the rule to
// this specific mapping only.
func (m *ControlManager) insertRoutingRule(mappingName string, proxy *config.Proxy) {
	ruleStr := fmt.Sprintf("MATCH,%s#%s", proxy.Name, mappingName)

	m.ruleConf.Lock()
	rs := strings.SplitN(ruleStr, ",", 3)
	if len(rs) >= 2 {
		matcher := config.NewMatchAllMatcher(strings.Trim(rs[1], "'"))
		m.ruleConf.Matchers = append([]config.Matcher{matcher}, m.ruleConf.Matchers...)
	}
	m.ruleConf.Unlock()
}

// HandleClose cleans up all resources for an address when control disconnects.
func (m *ControlManager) HandleClose(address string) {
	m.mu.Lock()
	session, hasSession := m.sessions[address]
	if hasSession {
		delete(m.sessions, address)
	}
	resource, hasResource := m.resources[address]
	if hasResource {
		delete(m.resources, address)
	}
	// m.resources is keyed by dynAddr, but HandleClose receives the control
	// address (SOCKS5/Trojan dstAddr). Use session.DynAddr to clean up.
	if session != nil && session.DynAddr != "" {
		if r, ok := m.resources[session.DynAddr]; ok {
			if resource == nil {
				resource = r
			}
			delete(m.resources, session.DynAddr)
		}
	}
	m.mu.Unlock()

	if session != nil {
		close(session.stopCh)
		session.Conn.Close()
		// Always update disconnected state so SSE pushes live updates.
		discID := session.ReverseID
		discSeq := session.Seq
		if discID == "" {
			discID = session.DynAddr
			discSeq = 0
		}
		m.bindingStore.SetDisconnected(discID, discSeq)
	}

	if resource != nil {
		util.LogInfo("[CONTROL] cleaning up resources for %s (port:%d, proto:%s)",
			address, resource.Port, resource.Proto)

		// Close listener
		if resource.Listener != nil {
			resource.Listener.Close()
		}

		// Remove proxy and its scoped MATCH rule from RuleConfiguration
		m.ruleConf.Lock()
		if resource.Proxy != nil {
			delete(m.ruleConf.ProxyNames, resource.Proxy.Name)
			// Remove from Proxies slice
			for i, p := range m.ruleConf.Proxies {
				if p == resource.Proxy {
					m.ruleConf.Proxies = append(m.ruleConf.Proxies[:i], m.ruleConf.Proxies[i+1:]...)
					break
				}
			}
		}
		// Remove the MATCH rule inserted by insertRoutingRule.
		if resource.MappingName != "" && resource.Proxy != nil {
			targetProxy := resource.Proxy.Name
			targetMap := resource.MappingName
			newMatchers := make([]config.Matcher, 0, len(m.ruleConf.Matchers))
			for _, matcher := range m.ruleConf.Matchers {
				if mam, ok := matcher.(*config.MatchAllMatcher); ok {
					if mam.ProxyName() == targetProxy && mam.MappingName() == targetMap {
						continue // skip the dynamic rule we created
					}
				}
				newMatchers = append(newMatchers, matcher)
			}
			m.ruleConf.Matchers = newMatchers
		}
		m.ruleConf.Unlock()

		// Close all data connections in Registry for this dyn address.
		// Registry bottoms are keyed by dynAddr, not the control address.
		reg := reverse.GlobalRegistry()
		if reg != nil && resource.Address != "" {
			reg.CloseByAddress(resource.Address)
		}
	}
}

// ForceRemoveBinding force-deletes a binding and cleans up any active session or dynamic resource.
func (m *ControlManager) ForceRemoveBinding(reverseID string, seq int) error {
	m.mu.Lock()
	// Find and remove any active session for this identity
	var sess *ControlSession
	for addr, s := range m.sessions {
		if s.ReverseID == reverseID && s.Seq == seq {
			sess = s
			delete(m.sessions, addr)
			break
		}
	}
	// Find and remove any dynamic resource for this identity
	var res *DynamicResource
	for addr, r := range m.resources {
		if r.ReverseID == reverseID && r.Seq == seq {
			res = r
			delete(m.resources, addr)
			break
		}
	}
	// Also check by DynAddr if session was found
	if sess != nil && sess.DynAddr != "" {
		if r, ok := m.resources[sess.DynAddr]; ok {
			if res == nil {
				res = r
			}
			delete(m.resources, sess.DynAddr)
		}
	}
	m.mu.Unlock()

	if sess != nil {
		close(sess.stopCh)
		sess.Conn.Close()
	}

	if res != nil {
		util.LogInfo("[CONTROL] force cleanup resources for %s#%d (port:%d, proto:%s)",
			reverseID, seq, res.Port, res.Proto)
		if res.Listener != nil {
			res.Listener.Close()
		}
		m.ruleConf.Lock()
		if res.Proxy != nil {
			delete(m.ruleConf.ProxyNames, res.Proxy.Name)
			for i, p := range m.ruleConf.Proxies {
				if p == res.Proxy {
					m.ruleConf.Proxies = append(m.ruleConf.Proxies[:i], m.ruleConf.Proxies[i+1:]...)
					break
				}
			}
		}
		if res.MappingName != "" && res.Proxy != nil {
			targetProxy := res.Proxy.Name
			targetMap := res.MappingName
			newMatchers := make([]config.Matcher, 0, len(m.ruleConf.Matchers))
			for _, matcher := range m.ruleConf.Matchers {
				if mam, ok := matcher.(*config.MatchAllMatcher); ok {
					if mam.ProxyName() == targetProxy && mam.MappingName() == targetMap {
						continue
					}
				}
				newMatchers = append(newMatchers, matcher)
			}
			m.ruleConf.Matchers = newMatchers
		}
		m.ruleConf.Unlock()
		reg := reverse.GlobalRegistry()
		if reg != nil && res.Address != "" {
			reg.CloseByAddress(res.Address)
		}
	}

	m.bindingStore.Remove(reverseID, seq)
	return nil
}

// CloseAll cleans up all sessions and resources (for shutdown).
func (m *ControlManager) CloseAll() {
	m.mu.Lock()
	addrs := make([]string, 0, len(m.sessions))
	for addr := range m.sessions {
		addrs = append(addrs, addr)
	}
	m.mu.Unlock()

	for _, addr := range addrs {
		m.HandleClose(addr)
	}
}

// handleControlConnection is the package-level entry point called from
// socks5.go and trojan.go when BIND PORT=1 is received.
func handleControlConnection(conn net.Conn, address string) {
	if GlobalControlManager != nil {
		GlobalControlManager.HandleControlConnection(conn, address)
	} else {
		util.LogError("[CONTROL] no ControlManager initialized, closing control conn from %s", conn.RemoteAddr())
		conn.Close()
	}
}

// generateDynAddress generates a unique address ID.
func generateDynAddress() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "dyn-" + hex.EncodeToString(b)
}
