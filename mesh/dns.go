package mesh

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"phaethon/util"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// DNSHijacker intercepts UDP:53 queries and returns Fake-IP responses.
type DNSHijacker struct {
	ns      *stack.Stack
	pool    *FakeIPPool
	tunAddr tcpip.Address
	dnsAddr tcpip.Address
	udpEP   tcpip.Endpoint
	wq      waiter.Queue
	started bool
	closeCh chan struct{}

	// Cross-node DNS forwarding
	resolveDomainSubnet func(domain string) (*net.IPNet, bool) // returns (subnet, needsFail)
	cache               *DNSCache

	// Async query processing
	dnsQueryCh chan dnsQuery // buffered channel for worker pool

	// Async DNS forwarder (v0.2.0)
	forwarderSocket tcpip.Endpoint            // persistent UDP socket for forwarding
	pendingQueries  map[string]chan dnsResult // domain -> callback channel
	pendingMu       sync.Mutex                // protects pendingQueries
	forwarderStopCh chan struct{}             // stops background listener
}

// dnsResult represents a DNS resolution result from the async forwarder.
type dnsResult struct {
	ip  net.IP
	ttl time.Duration
	err error
}

type dnsQuery struct {
	packet     []byte
	remoteAddr tcpip.FullAddress
}

// DNSCache caches domain → Fake-IP mappings from remote DNS hijackers.
type DNSCache struct {
	mu      sync.RWMutex
	entries map[string]*dnsCacheEntry
}

type dnsCacheEntry struct {
	fakeIP net.IP
	expiry time.Time
}

// NewDNSCache creates an empty DNS cache.
func NewDNSCache() *DNSCache {
	return &DNSCache{
		entries: make(map[string]*dnsCacheEntry),
	}
}

// Get returns the cached Fake-IP for a domain, or nil if not cached or expired.
func (c *DNSCache) Get(domain string) net.IP {
	c.mu.RLock()
	e, ok := c.entries[domain]
	c.mu.RUnlock()
	if !ok || time.Now().After(e.expiry) {
		return nil
	}
	return e.fakeIP
}

// Set stores a domain → Fake-IP mapping with a TTL.
func (c *DNSCache) Set(domain string, fakeIP net.IP, ttl time.Duration) {
	c.mu.Lock()
	c.entries[domain] = &dnsCacheEntry{
		fakeIP: fakeIP,
		expiry: time.Now().Add(ttl),
	}
	c.mu.Unlock()
}

// NewDNSHijacker creates a DNS hijacker. Netstack binding is deferred to BindNetstack.
func NewDNSHijacker(ns *stack.Stack, pool *FakeIPPool, tunAddr, dnsAddr tcpip.Address) *DNSHijacker {
	return &DNSHijacker{
		ns:              ns,
		pool:            pool,
		tunAddr:         tunAddr,
		dnsAddr:         dnsAddr,
		closeCh:         make(chan struct{}),
		cache:           NewDNSCache(),
		dnsQueryCh:      make(chan dnsQuery, 1024),
		pendingQueries:  make(map[string]chan dnsResult),
		forwarderStopCh: make(chan struct{}),
	}
}

// BindNetstack sets the netstack and addresses for the DNS hijacker.
// Called when TUN engine is initialized and can provide the netstack.
func (h *DNSHijacker) BindNetstack(ns *stack.Stack, tunAddr, dnsAddr tcpip.Address) {
	h.ns = ns
	h.tunAddr = tunAddr
	h.dnsAddr = dnsAddr
	
	// Initialize async DNS forwarder
	h.startForwarder()
}

// startForwarder initializes the persistent UDP socket and starts the background listener.
// The listener continuously receives DNS responses and caches them, triggering callbacks
// for any pending queries.
func (h *DNSHijacker) startForwarder() {
	if h.forwarderSocket != nil {
		return // already started
	}
	
	// Create persistent UDP endpoint for forwarding
	var wq waiter.Queue
	ep, err := h.ns.NewEndpoint(udp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
	if err != nil {
		util.LogModuleWarn("dns", "[DNS-FWD] failed to create forwarder endpoint: %v", err)
		return
	}
	
	// Bind to DNS address (for receiving responses)
	if err := ep.Bind(tcpip.FullAddress{NIC: 1, Addr: h.dnsAddr, Port: 0}); err != nil {
		util.LogModuleWarn("dns", "[DNS-FWD] failed to bind forwarder endpoint: %v", err)
		ep.Close()
		return
	}
	
	h.forwarderSocket = ep
	util.LogModuleInfo("dns", "[DNS-FWD] async forwarder started")
	
	// Start background listener
	go h.forwarderListener()
}

// forwarderListener continuously reads DNS responses from the persistent socket,
// caches them, and triggers callbacks for pending queries.
func (h *DNSHijacker) forwarderListener() {
	defer func() {
		if r := recover(); r != nil {
			util.LogModuleError("dns", "[DNS-FWD] listener panic: %v", r)
			// Restart listener after panic
			time.Sleep(time.Second)
			h.forwarderStopCh = make(chan struct{})
			go h.forwarderListener()
		}
	}()
	
	for {
		select {
		case <-h.forwarderStopCh:
			util.LogModuleInfo("dns", "[DNS-FWD] listener stopped")
			return
		default:
		}
		
		// Read response (blocks until data arrives or error)
		var buf bytes.Buffer
		res, err := h.forwarderSocket.Read(&buf, tcpip.ReadOptions{NeedRemoteAddr: true})
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue // timeout, keep listening
			}
			util.LogModuleWarn("dns", "[DNS-FWD] read error: %v", err)
			time.Sleep(100 * time.Millisecond) // avoid busy loop on error
			continue
		}
		
		if res.Total < 12 {
			continue // too short to be a valid DNS response
		}
		
		// Parse response
		domain, ip, ttl := parseDNSResponseForCache(buf.Bytes())
		if domain == "" || ip == nil {
			continue
		}
		
		util.LogModuleDebug("dns", "[DNS-FWD] received response: domain=%s ip=%s ttl=%v", domain, ip, ttl)
		
		// 1. Cache the response
		h.cache.Set(domain, ip, ttl)
		
		// 2. Trigger callback if client is waiting
		h.pendingMu.Lock()
		if ch, ok := h.pendingQueries[domain]; ok {
			select {
			case ch <- dnsResult{ip: ip, ttl: ttl}:
				util.LogModuleDebug("dns", "[DNS-FWD] triggered callback for %s", domain)
			default:
				util.LogModuleWarn("dns", "[DNS-FWD] callback channel full for %s", domain)
			}
			delete(h.pendingQueries, domain)
		}
		h.pendingMu.Unlock()
	}
}

// parseDNSResponseForCache extracts domain, IP, and TTL from a DNS response.
func parseDNSResponseForCache(resp []byte) (domain string, ip net.IP, ttl time.Duration) {
	// Reuse existing ParseDNSResponseIP logic
	parsedIP, parsedTTL := ParseDNSResponseIP(resp)
	if parsedIP == nil {
		return "", nil, 0
	}
	
	// Extract domain from response (question section)
	domain, _, _ = parseDNSQueryDomain(resp)
	
	return domain, parsedIP, parsedTTL
}

// IsBound returns true if the netstack has been bound.
func (h *DNSHijacker) IsBound() bool {
	return h.ns != nil
}

// SetDomainResolver registers a callback that returns the remote Fake-IP subnet
// for a domain. Returns (subnet, needsFail):
//   - subnet != nil: matched a route, forward to remote
//   - subnet == nil && needsFail == true: static match but node not ready, return SERVFAIL
//   - subnet == nil && needsFail == false: no match, fallback to local pool
func (h *DNSHijacker) SetDomainResolver(resolver func(domain string) (*net.IPNet, bool)) {
	h.resolveDomainSubnet = resolver
}

// Start binds a UDP socket on port 53 inside netstack and starts the serve loop.
// The wg is used to track the serve goroutine's lifetime.
func (h *DNSHijacker) Start(wg *sync.WaitGroup) error {
	util.LogModuleInfo("dns", "[DNS-DEBUG] DNSHijacker.Start() called, dnsAddr=%s", h.dnsAddr)
	var err tcpip.Error
	h.udpEP, err = h.ns.NewEndpoint(udp.ProtocolNumber, ipv4.ProtocolNumber, &h.wq)
	if err != nil {
		return fmt.Errorf("new udp endpoint: %v", err)
	}
	h.started = true

	addr := tcpip.FullAddress{
		Addr: h.dnsAddr,
		Port: 53,
	}
	if err := h.udpEP.Bind(addr); err != nil {
		return fmt.Errorf("bind udp 53 on %s: %v", h.dnsAddr, err)
	}
	util.LogModuleInfo("dns", "[DNS-DEBUG] DNSHijacker bound to %s:53", h.dnsAddr)

	// Start worker pool for DNS query processing
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.dnsWorkerLoop()
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		h.serveLoop()
	}()
	return nil
}

// Stop closes the UDP endpoint and signals serveLoop to exit.
func (h *DNSHijacker) Stop() {
	if h.started {
		close(h.closeCh)
		if h.udpEP != nil {
			h.udpEP.Close()
		}
	}
}

// Resolve returns the raw DNS response bytes for a query without sending it
// through the netstack. It is used by the Windows-side DNS proxy and internal
// health probes so they do not depend on gVisor loopback delivery semantics.
// It returns a Fake-IP without resolving the real IP. The real IP will be
// resolved at connection time when the rule matches DIRECT.
func (h *DNSHijacker) Resolve(query []byte) ([]byte, error) {
	if len(query) == 0 {
		return nil, fmt.Errorf("empty query")
	}
	domain, qtype, ok := parseDNSQueryDomain(query)
	if !ok || domain == "" {
		return nil, fmt.Errorf("failed to parse query")
	}

	// Return empty response for AAAA queries (no IPv6 support)
	if qtype == 28 { // AAAA
		resp := buildDNSEmptyResponse(query)
		if resp == nil {
			return nil, fmt.Errorf("failed to build empty response")
		}
		return resp, nil
	}

	fakeIP := h.pool.Lookup(domain)
	util.LogModuleDebug("dns", "tun dns: %s -> fake=%s", domain, fakeIP)

	resp := buildDNSResponse(query, fakeIP.To4())
	if resp == nil {
		return nil, fmt.Errorf("failed to build response")
	}
	return resp, nil
}

// dnsWorkerLoop processes DNS queries from dnsQueryCh.
// Part of the worker pool for bounded concurrent DNS processing.
func (h *DNSHijacker) dnsWorkerLoop() {
	for {
		select {
		case <-h.closeCh:
			return
		case q := <-h.dnsQueryCh:
			h.processQuery(q.packet, q.remoteAddr)
		}
	}
}

func (h *DNSHijacker) serveLoop() {
	waitEntry, ch := waiter.NewChannelEntry(waiter.EventIn)
	h.wq.EventRegister(&waitEntry)
	defer h.wq.EventUnregister(&waitEntry)

	util.LogModuleInfo("dns", "[DNS-DEBUG] DNSHijacker serveLoop started")

	for {
		var buf bytes.Buffer
		res, err := h.udpEP.Read(&buf, tcpip.ReadOptions{NeedRemoteAddr: true})
		if err != nil {
			if _, ok := err.(*tcpip.ErrWouldBlock); ok {
				select {
				case <-ch:
				case <-h.closeCh:
					return
				}
				continue
			}
			util.LogModuleWarn("dns", "tun dns: read error: %v", err)
			return
		}

		packet := buf.Bytes()
		if res.Total == 0 {
			continue
		}

		// Debug: log every received packet
		srcIP := net.IP(res.RemoteAddr.Addr.AsSlice())
		srcPort := res.RemoteAddr.Port
		if ForwardingTraceEnabled() {
			util.LogModuleInfo("dns", "[DNS-DEBUG] DNSHijacker received packet: from=%s:%d len=%d", srcIP, srcPort, res.Total)
		}

		// Minimal DNS parsing: extract the queried domain for logging
		domain, _, ok := parseDNSQueryDomain(packet)
		if ok && domain != "" && ForwardingTraceEnabled() {
			util.LogModuleInfo("dns", "[DNS-DEBUG] DNSHijacker: query domain=%s from=%s:%d", domain, srcIP, srcPort)
		}

		// Queue for async processing by worker pool
		packetCopy := make([]byte, len(packet))
		copy(packetCopy, packet)
		select {
		case h.dnsQueryCh <- dnsQuery{packet: packetCopy, remoteAddr: res.RemoteAddr}:
			// Queued successfully
		default:
			util.LogModuleDebug("dns", "[DNS] dnsQueryCh full, dropping query")
		}
	}
}

// processQuery handles a single DNS query: cache check, local pool, or remote forward.
func (h *DNSHijacker) processQuery(packet []byte, remoteAddr tcpip.FullAddress) {
	domain, qtype, ok := parseDNSQueryDomain(packet)
	if !ok || domain == "" {
		util.LogModuleWarn("dns", "tun dns: failed to parse query from %d bytes", len(packet))
		return
	}

	util.LogModuleInfo("dns", "[DNS-DEBUG] processQuery: domain=%s from=%s:%d", domain, remoteAddr.Addr, remoteAddr.Port)

	// Return empty response for AAAA queries (no IPv6 support)
	if qtype == 28 { // AAAA
		util.LogModuleDebug("dns", "tun dns: AAAA query for %s, returning empty response", domain)
		resp := buildDNSEmptyResponse(packet)
		if resp != nil {
			if _, err := h.udpEP.Write(&SlicePayload{Data: resp}, tcpip.WriteOptions{To: &remoteAddr}); err != nil {
				util.LogModuleWarn("dns", "tun dns: write empty response for %s to %s:%d fail: %v", domain, remoteAddr.Addr, remoteAddr.Port, err)
			} else {
				util.LogModuleInfo("dns", "[DNS-DEBUG] processQuery: sent AAAA empty response for %s to %s:%d", domain, remoteAddr.Addr, remoteAddr.Port)
			}
		}
		return
	}

	// Check cache first
	if cachedIP := h.cache.Get(domain); cachedIP != nil {
		util.LogModuleDebug("dns", "tun dns: %s -> %s (cached)", domain, cachedIP)
		resp := buildDNSResponse(packet, cachedIP.To4())
		if resp != nil {
			if _, err := h.udpEP.Write(&SlicePayload{Data: resp}, tcpip.WriteOptions{To: &remoteAddr}); err != nil {
				util.LogModuleWarn("dns", "tun dns: write cached response for %s to %s:%d fail: %v", domain, remoteAddr.Addr, remoteAddr.Port, err)
			} else {
				util.LogModuleInfo("dns", "[DNS-DEBUG] processQuery: sent cached response for %s to %s:%d", domain, remoteAddr.Addr, remoteAddr.Port)
			}
		}
		return
	}

	// Check if domain belongs to a remote node
	if h.resolveDomainSubnet != nil {
		util.LogModuleInfo("dns", "[DNS-FWD-DEBUG] resolveDomainSubnet called: domain=%s", domain)
		remoteSubnet, needsFail := h.resolveDomainSubnet(domain)
		util.LogModuleInfo("dns", "[DNS-FWD-DEBUG] resolveDomainSubnet result: domain=%s remoteSubnet=%v needsFail=%v", domain, remoteSubnet, needsFail)
		if remoteSubnet != nil {
			// Matched a route (static or dynamic), forward to remote
			util.LogModuleInfo("dns", "[DNS-FWD-DEBUG] FORWARDING: domain=%s to subnet=%s", domain, remoteSubnet)
			remoteIP, ttl, err := h.forwardToRemote(remoteSubnet, packet)
			if err != nil {
				util.LogModuleWarn("dns", "[DNS-FWD-DEBUG] FORWARD FAILED: domain=%s err=%v", domain, err)
				// Return SERVFAIL instead of fallback to local pool
				// This allows the client to retry and get the correct IP once mesh recovers
				resp := buildDNSErrorResponse(packet)
				if resp != nil {
					if _, err := h.udpEP.Write(&SlicePayload{Data: resp}, tcpip.WriteOptions{To: &remoteAddr}); err != nil {
						util.LogModuleWarn("dns", "tun dns: write SERVFAIL for %s to %s:%d fail: %v", domain, remoteAddr.Addr, remoteAddr.Port, err)
					} else {
						util.LogModuleInfo("dns", "[DNS-FWD-DEBUG] SENT SERVFAIL: domain=%s to %s:%d", domain, remoteAddr.Addr, remoteAddr.Port)
					}
				}
				return
			}
			h.cache.Set(domain, remoteIP, ttl)
			util.LogModuleInfo("dns", "[DNS-FWD-DEBUG] FORWARD SUCCESS: domain=%s -> %s ttl=%v", domain, remoteIP, ttl)
			resp := buildDNSResponse(packet, remoteIP.To4())
			if resp != nil {
				if _, err := h.udpEP.Write(&SlicePayload{Data: resp}, tcpip.WriteOptions{To: &remoteAddr}); err != nil {
					util.LogModuleWarn("dns", "tun dns: write remote response for %s to %s:%d fail: %v", domain, remoteAddr.Addr, remoteAddr.Port, err)
				} else {
					util.LogModuleInfo("dns", "[DNS-FWD-DEBUG] SENT RESPONSE: domain=%s -> %s to %s:%d", domain, remoteIP, remoteAddr.Addr, remoteAddr.Port)
				}
			}
			return
		} else if needsFail {
			// Static match but node not ready → SERVFAIL
			util.LogModuleWarn("dns", "[DNS-FWD-DEBUG] NEEDSFAIL: domain=%s matched static route but node not ready, returning SERVFAIL", domain)
			resp := buildDNSErrorResponse(packet)
			if resp != nil {
				if _, err := h.udpEP.Write(&SlicePayload{Data: resp}, tcpip.WriteOptions{To: &remoteAddr}); err != nil {
					util.LogModuleWarn("dns", "tun dns: write SERVFAIL for %s to %s:%d fail: %v", domain, remoteAddr.Addr, remoteAddr.Port, err)
				} else {
					util.LogModuleInfo("dns", "[DNS-FWD-DEBUG] SENT SERVFAIL (needsFail): domain=%s to %s:%d", domain, remoteAddr.Addr, remoteAddr.Port)
				}
			}
			return
		}
		// else: no match, fallback to local pool
		util.LogModuleInfo("dns", "[DNS-FWD-DEBUG] NO MATCH: domain=%s fallback to local pool", domain)
	} else {
		util.LogModuleInfo("dns", "[DNS-FWD-DEBUG] NO RESOLVER: resolveDomainSubnet is nil, using local pool", domain)
	}

	// Local pool resolution (default)
	fakeIP := h.pool.Lookup(domain)
	util.LogModuleDebug("dns", "tun dns: %s -> %s", domain, fakeIP)
	resp := buildDNSResponse(packet, fakeIP.To4())

	if resp == nil {
		return
	}
	if _, err := h.udpEP.Write(&SlicePayload{Data: resp}, tcpip.WriteOptions{To: &remoteAddr}); err != nil {
		util.LogModuleWarn("dns", "tun dns: write response to %s:%d fail: %v", remoteAddr.Addr, remoteAddr.Port, err)
	} else {
		util.LogModuleDebug("dns", "tun dns: %s -> response sent (%d bytes)", domain, len(resp))
	}
}

// forwardToRemote sends a DNS query to the remote node's DNS hijacker via a
// gvisor netstack UDP socket. The packet flows: netstack → writeLoop → mesh
// routing → remote HandleMeshFrame → GIP path → InjectMeshPacket → remote DNS
// hijacker. The response follows the reverse path.
// forwardToRemote sends a DNS query to the remote node's DNS hijacker using the
// async forwarder. The query is sent via the persistent socket, and we wait for
// the background listener to receive the response and trigger our callback.
// Even if we timeout, the response will be cached for future queries.
func (h *DNSHijacker) forwardToRemote(remoteSubnet *net.IPNet, query []byte) (net.IP, time.Duration, error) {
	startTime := time.Now()
	
	// Derive remote GIP (.3) from subnet
	baseIP := remoteSubnet.IP.To4()
	if baseIP == nil {
		return nil, 0, fmt.Errorf("invalid remote subnet")
	}
	remoteGIP := make(net.IP, 4)
	copy(remoteGIP, baseIP)
	remoteGIP[3] |= 3
	
	// Extract domain from query
	domain, _, _ := parseDNSQueryDomain(query)
	if domain == "" {
		return nil, 0, fmt.Errorf("no domain in query")
	}
	
	util.LogModuleInfo("dns", "[DNS-FWD] START: domain=%s remoteGIP=%s", domain, remoteGIP)
	
	// 1. Check cache (might have been cached by background listener from a late response)
	if cachedIP := h.cache.Get(domain); cachedIP != nil {
		util.LogModuleInfo("dns", "[DNS-FWD] CACHE HIT: domain=%s ip=%s (late response cached)", domain, cachedIP)
		return cachedIP, time.Hour, nil // TTL doesn't matter much for cached
	}
	
	// 2. Ensure forwarder is initialized
	if h.forwarderSocket == nil {
		util.LogModuleWarn("dns", "[DNS-FWD] forwarder not initialized, falling back to sync")
		return h.forwardToRemoteSync(remoteSubnet, query)
	}
	
	// 3. Register callback
	resultCh := make(chan dnsResult, 1)
	h.pendingMu.Lock()
	h.pendingQueries[domain] = resultCh
	h.pendingMu.Unlock()
	
	// Ensure cleanup on exit
	defer func() {
		h.pendingMu.Lock()
		delete(h.pendingQueries, domain)
		h.pendingMu.Unlock()
	}()
	
	// 4. Send query via persistent socket
	remoteAddr := tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFromSlice(remoteGIP), Port: 53}
	queryCopy := make([]byte, len(query))
	copy(queryCopy, query)
	
	writeStart := time.Now()
	if _, err := h.forwarderSocket.Write(&SlicePayload{Data: queryCopy}, tcpip.WriteOptions{To: &remoteAddr}); err != nil {
		util.LogModuleWarn("dns", "[DNS-FWD] WRITE FAILED: domain=%s err=%v", domain, err)
		return nil, 0, fmt.Errorf("write query: %v", err)
	}
	util.LogModuleDebug("dns", "[DNS-FWD] WRITE OK: domain=%s duration=%v", domain, time.Since(writeStart))
	
	// 5. Wait for callback (with timeout)
	const timeout = 5 * time.Second
	select {
	case result := <-resultCh:
		if result.err != nil {
			return nil, 0, result.err
		}
		util.LogModuleInfo("dns", "[DNS-FWD] COMPLETE: domain=%s ip=%s ttl=%v duration=%v", 
			domain, result.ip, result.ttl, time.Since(startTime))
		return result.ip, result.ttl, nil
		
	case <-time.After(timeout):
		// Timeout, but background listener continues and will cache the late response
		util.LogModuleWarn("dns", "[DNS-FWD] TIMEOUT: domain=%s timeout=%v duration=%v (response will be cached if arrives late)", 
			domain, timeout, time.Since(startTime))
		return nil, 0, fmt.Errorf("timeout after %v", timeout)
	}
}

// forwardToRemoteSync is the fallback synchronous forwarding (used if async forwarder not initialized).
func (h *DNSHijacker) forwardToRemoteSync(remoteSubnet *net.IPNet, query []byte) (net.IP, time.Duration, error) {
	// Derive remote GIP (.3) from subnet
	baseIP := remoteSubnet.IP.To4()
	if baseIP == nil {
		return nil, 0, fmt.Errorf("invalid remote subnet")
	}
	remoteGIP := make(net.IP, 4)
	copy(remoteGIP, baseIP)
	remoteGIP[3] |= 3
	
	domain, _, _ := parseDNSQueryDomain(query)
	
	remoteAddr := tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFromSlice(remoteGIP), Port: 53}
	localAddr := tcpip.FullAddress{NIC: 1, Addr: h.dnsAddr}
	
	const timeout = 5 * time.Second
	conn, err := gonet.DialUDP(h.ns, &localAddr, &remoteAddr, ipv4.ProtocolNumber)
	if err != nil {
		return nil, 0, fmt.Errorf("dial %s:53: %v", remoteGIP, err)
	}
	defer conn.Close()
	
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return nil, 0, fmt.Errorf("set deadline: %v", err)
	}
	
	if _, err := conn.Write(query); err != nil {
		return nil, 0, fmt.Errorf("write query: %v", err)
	}
	
	resp := make([]byte, 512)
	n, err := conn.Read(resp)
	if err != nil {
		return nil, 0, fmt.Errorf("read response: %v", err)
	}
	
	respIP, ttl := ParseDNSResponseIP(resp[:n])
	if respIP == nil {
		return nil, 0, fmt.Errorf("no A record in response")
	}
	
	util.LogModuleInfo("dns", "[DNS-FWD-SYNC] domain=%s ip=%s ttl=%v", domain, respIP, ttl)
	return respIP, ttl, nil
}


// SlicePayload implements tcpip.Payload for byte slices.
type SlicePayload struct {
	Data []byte
}

func (p *SlicePayload) Len() int { return len(p.Data) }
func (p *SlicePayload) Read(dst []byte) (int, error) {
	n := copy(dst, p.Data)
	p.Data = p.Data[n:]
	if len(p.Data) == 0 {
		return n, io.EOF
	}
	return n, nil
}

// parseDNSQueryDomain extracts the queried domain and query type from a DNS query packet.
// Query type: 1=A, 28=AAAA, etc.
func parseDNSQueryDomain(pkt []byte) (string, uint16, bool) {
	if len(pkt) < 12 {
		return "", 0, false
	}
	flags := (uint16(pkt[2]) << 8) | uint16(pkt[3])
	if flags&0x8000 != 0 {
		return "", 0, false
	}
	qdcount := (uint16(pkt[4]) << 8) | uint16(pkt[5])
	if qdcount == 0 {
		return "", 0, false
	}

	off := 12
	var labels []string
	for {
		if off >= len(pkt) {
			return "", 0, false
		}
		llen := int(pkt[off])
		off++
		if llen == 0 {
			break
		}
		if llen > 63 || off+llen > len(pkt) {
			return "", 0, false
		}
		labels = append(labels, string(pkt[off:off+llen]))
		off += llen
	}

	// Read QTYPE and QCLASS (4 bytes total after domain)
	if off+4 > len(pkt) {
		return "", 0, false
	}
	qtype := (uint16(pkt[off]) << 8) | uint16(pkt[off+1])

	var domain string
	for i, l := range labels {
		if i > 0 {
			domain += "."
		}
		domain += l
	}
	return domain, qtype, true
}

// buildDNSResponse builds a minimal DNS response with a single A record.
func buildDNSResponse(query []byte, ip net.IP) []byte {
	if len(query) < 12 {
		return nil
	}
	resp := make([]byte, 0, 512)
	resp = append(resp, query[:2]...)  // transaction ID
	resp = append(resp, 0x81, 0x80)    // flags: response, no error
	resp = append(resp, query[4:6]...) // QDCOUNT
	resp = append(resp, 0x00, 0x01)    // ANCOUNT = 1
	resp = append(resp, 0x00, 0x00)    // NSCOUNT
	resp = append(resp, 0x00, 0x00)    // ARCOUNT

	qoff := 12
	for {
		if qoff >= len(query) {
			return nil
		}
		llen := int(query[qoff])
		resp = append(resp, query[qoff])
		qoff++
		if llen == 0 {
			break
		}
		resp = append(resp, query[qoff:qoff+llen]...)
		qoff += llen
	}
	if qoff+4 > len(query) {
		return nil
	}
	resp = append(resp, query[qoff:qoff+4]...)
	qoff += 4

	resp = append(resp, 0xc0, 0x0c)             // pointer to offset 12
	resp = append(resp, 0x00, 0x01)             // Type A
	resp = append(resp, 0x00, 0x01)             // Class IN
	resp = append(resp, 0x00, 0x00, 0x00, 0x05) // TTL 5
	resp = append(resp, 0x00, 0x04)             // RDLENGTH
	resp = append(resp, ip...)
	return resp
}

// buildDNSErrorResponse builds a SERVFAIL DNS response.
func buildDNSErrorResponse(query []byte) []byte {
	if len(query) < 12 {
		return nil
	}
	resp := make([]byte, 0, len(query))
	// Transaction ID
	resp = append(resp, query[0], query[1])
	// Flags: response + SERVFAIL (RCODE=2)
	resp = append(resp, 0x80, 0x02)
	// QDCOUNT=1, ANCOUNT=0, NSCOUNT=0, ARCOUNT=0
	resp = append(resp, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00)
	// Copy question section
	qoff := 12
	for qoff < len(query) {
		llen := int(query[qoff])
		if llen == 0 {
			resp = append(resp, 0x00)
			qoff++
			break
		}
		if qoff+1+llen > len(query) {
			return nil
		}
		resp = append(resp, query[qoff:qoff+1+llen]...)
		qoff += 1 + llen
	}
	// Copy QTYPE and QCLASS
	if qoff+4 <= len(query) {
		resp = append(resp, query[qoff:qoff+4]...)
	}
	return resp
}

// buildDNSEmptyResponse builds a NOERROR DNS response with no answers (for AAAA queries).
func buildDNSEmptyResponse(query []byte) []byte {
	if len(query) < 12 {
		return nil
	}
	resp := make([]byte, 0, len(query))
	// Transaction ID
	resp = append(resp, query[0], query[1])
	// Flags: response, no error (NOERROR)
	resp = append(resp, 0x81, 0x80)
	// QDCOUNT=1, ANCOUNT=0, NSCOUNT=0, ARCOUNT=0
	resp = append(resp, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00)
	// Copy question section
	qoff := 12
	for qoff < len(query) {
		llen := int(query[qoff])
		if llen == 0 {
			resp = append(resp, 0x00)
			qoff++
			break
		}
		if qoff+1+llen > len(query) {
			return nil
		}
		resp = append(resp, query[qoff:qoff+1+llen]...)
		qoff += 1 + llen
	}
	// Copy QTYPE and QCLASS
	if qoff+4 <= len(query) {
		resp = append(resp, query[qoff:qoff+4]...)
	}
	return resp
}

// BuildDNSQuery builds a minimal DNS A query for domain using txID.
func BuildDNSQuery(domain string, txID uint16) []byte {
	pkt := make([]byte, 0, 512)
	pkt = append(pkt, byte(txID>>8), byte(txID))
	pkt = append(pkt, 0x01, 0x00) // flags: standard query, recursion desired
	pkt = append(pkt, 0x00, 0x01) // QDCOUNT = 1
	pkt = append(pkt, 0x00, 0x00) // ANCOUNT
	pkt = append(pkt, 0x00, 0x00) // NSCOUNT
	pkt = append(pkt, 0x00, 0x00) // ARCOUNT

	for _, label := range strings.Split(domain, ".") {
		pkt = append(pkt, byte(len(label)))
		pkt = append(pkt, []byte(label)...)
	}
	pkt = append(pkt, 0x00)       // end of name
	pkt = append(pkt, 0x00, 0x01) // Type A
	pkt = append(pkt, 0x00, 0x01) // Class IN
	return pkt
}

// ParseDNSResponseIP extracts the first A record IPv4 address and TTL from a
// DNS response. Returns (nil, 0) if the response is invalid or not an A record.
func ParseDNSResponseIP(resp []byte) (net.IP, time.Duration) {
	if len(resp) < 12 {
		return nil, 0
	}
	flags := (uint16(resp[2]) << 8) | uint16(resp[3])
	if flags&0x8000 == 0 { // not a response
		return nil, 0
	}
	if flags&0x000f != 0 { // RCODE != 0
		return nil, 0
	}
	ancount := (uint16(resp[6]) << 8) | uint16(resp[7])
	if ancount == 0 {
		return nil, 0
	}

	// Skip question section.
	off := 12
	for {
		if off >= len(resp) {
			return nil, 0
		}
		llen := int(resp[off])
		off++
		if llen == 0 {
			break
		}
		if llen&0xc0 == 0xc0 { // compression pointer
			off++
			break
		}
		if llen > 63 || off+llen > len(resp) {
			return nil, 0
		}
		off += llen
	}
	off += 4 // QTYPE + QCLASS

	// Parse first answer.
	if off >= len(resp) {
		return nil, 0
	}
	if resp[off]&0xc0 == 0xc0 {
		off += 2
	} else {
		for {
			if off >= len(resp) {
				return nil, 0
			}
			llen := int(resp[off])
			off++
			if llen == 0 {
				break
			}
			if llen&0xc0 == 0xc0 {
				off++
				break
			}
			if llen > 63 || off+llen > len(resp) {
				return nil, 0
			}
			off += llen
		}
	}
	if off+10 > len(resp) {
		return nil, 0
	}
	rtype := (uint16(resp[off]) << 8) | uint16(resp[off+1])
	ttl := time.Duration((uint32(resp[off+4])<<24)|(uint32(resp[off+5])<<16)|(uint32(resp[off+6])<<8)|uint32(resp[off+7])) * time.Second
	rdlen := (uint16(resp[off+8]) << 8) | uint16(resp[off+9])
	off += 10
	if rtype != 0x0001 || rdlen != 4 {
		return nil, 0
	}
	if off+4 > len(resp) {
		return nil, 0
	}
	return net.IP(resp[off : off+4]), ttl
}

// ParseDNSResponseIPs extracts all A record IPv4 addresses from a DNS response.
// Returns empty slice if the response is invalid or contains no A records.
func ParseDNSResponseIPs(resp []byte) []net.IP {
	var ips []net.IP
	if len(resp) < 12 {
		return ips
	}
	flags := (uint16(resp[2]) << 8) | uint16(resp[3])
	if flags&0x8000 == 0 { // not a response
		return ips
	}
	if flags&0x000f != 0 { // RCODE != 0
		return ips
	}
	ancount := (uint16(resp[6]) << 8) | uint16(resp[7])
	if ancount == 0 {
		return ips
	}

	// Skip question section.
	off := 12
	for {
		if off >= len(resp) {
			return ips
		}
		llen := int(resp[off])
		off++
		if llen == 0 {
			break
		}
		if llen&0xc0 == 0xc0 { // compression pointer
			off++
			break
		}
		if llen > 63 || off+llen > len(resp) {
			return ips
		}
		off += llen
	}
	off += 4 // QTYPE + QCLASS

	// Parse all answers.
	for i := 0; i < int(ancount) && off < len(resp); i++ {
		// Skip name
		if off >= len(resp) {
			break
		}
		if resp[off]&0xc0 == 0xc0 {
			off += 2
		} else {
			for {
				if off >= len(resp) {
					return ips
				}
				llen := int(resp[off])
				off++
				if llen == 0 {
					break
				}
				if llen&0xc0 == 0xc0 {
					off++
					break
				}
				if llen > 63 || off+llen > len(resp) {
					return ips
				}
				off += llen
			}
		}
		if off+10 > len(resp) {
			break
		}
		rtype := (uint16(resp[off]) << 8) | uint16(resp[off+1])
		rdlen := (uint16(resp[off+8]) << 8) | uint16(resp[off+9])
		off += 10
		if rtype == 0x0001 && rdlen == 4 && off+4 <= len(resp) {
			ips = append(ips, net.IP(resp[off:off+4]))
		}
		off += int(rdlen)
	}
	return ips
}
