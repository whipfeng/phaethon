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

	// Async DNS forwarder (v0.2.0, fully async since dns_forward_full_async)
	forwarderSocket tcpip.Endpoint       // persistent UDP socket for forwarding
	forwarderWQ     waiter.Queue         // waiter queue for forwarder socket
	pendingQueries  map[string]*pendingQuery // domain (lowercase) -> waiting client
	pendingMu       sync.Mutex           // protects pendingQueries
	forwarderStopCh chan struct{}        // stops background listener
}

// forwardTimeout is how long a forwarded query waits for a response before
// the sweeper answers SERVFAIL to the client.
const forwardTimeout = 5 * time.Second

// pendingQuery tracks a client waiting on a forwarded DNS resolution.
// The forwarder listener responds directly to the client when the answer arrives.
type pendingQuery struct {
	query      []byte            // original query packet (carries the client TXID)
	remoteAddr tcpip.FullAddress // client to respond to
	at         time.Time         // registration time, for sweep expiry
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
	e, ok := c.entries[strings.ToLower(domain)]
	c.mu.RUnlock()
	if !ok || time.Now().After(e.expiry) {
		return nil
	}
	return e.fakeIP
}

// Set stores a domain → Fake-IP mapping with a TTL.
func (c *DNSCache) Set(domain string, fakeIP net.IP, ttl time.Duration) {
	c.mu.Lock()
	c.entries[strings.ToLower(domain)] = &dnsCacheEntry{
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
		pendingQueries:  make(map[string]*pendingQuery),
		forwarderStopCh: make(chan struct{}),
	}
}

// BindNetstack sets the netstack and addresses for the DNS hijacker.
// Called when TUN engine is initialized and can provide the netstack.
func (h *DNSHijacker) BindNetstack(ns *stack.Stack, tunAddr, dnsAddr tcpip.Address) {
	util.LogModuleInfo("dns", "[DNS-FWD] BindNetstack called, dnsAddr=%s", dnsAddr)
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
	ep, err := h.ns.NewEndpoint(udp.ProtocolNumber, ipv4.ProtocolNumber, &h.forwarderWQ)
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
	
	util.LogModuleInfo("dns", "[DNS-FWD] listener started, socket=%v dnsAddr=%s", h.forwarderSocket != nil, h.dnsAddr)
	
	// Register waiter for blocking reads
	waitEntry, ch := waiter.NewChannelEntry(waiter.EventIn)
	h.forwarderWQ.EventRegister(&waitEntry)
	defer h.forwarderWQ.EventUnregister(&waitEntry)
	
	readCount := 0
	sweepTicker := time.NewTicker(500 * time.Millisecond)
	defer sweepTicker.Stop()
	for {
		// Read response (returns ErrWouldBlock when no data available)
		var buf bytes.Buffer
		res, err := h.forwarderSocket.Read(&buf, tcpip.ReadOptions{NeedRemoteAddr: true})
		if err != nil {
			if _, ok := err.(*tcpip.ErrWouldBlock); ok {
				// No data available, block until notification, sweep tick, or stop signal
				select {
				case <-ch:
					continue
				case <-sweepTicker.C:
					h.sweepPending()
					continue
				case <-h.forwarderStopCh:
					util.LogModuleInfo("dns", "[DNS-FWD] listener stopped")
					return
				}
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue // timeout, keep listening
			}
			util.LogModuleWarn("dns", "[DNS-FWD] read error: %v", err)
			time.Sleep(100 * time.Millisecond) // avoid busy loop on error
			continue
		}
		
		readCount++
		util.LogModuleInfo("dns", "[DNS-FWD] received packet #%d: len=%d from=%s:%d", readCount, res.Total, res.RemoteAddr.Addr, res.RemoteAddr.Port)
		
		if res.Total < 12 {
			util.LogModuleWarn("dns", "[DNS-FWD] packet too short: %d bytes", res.Total)
			continue // too short to be a valid DNS response
		}
		
		// Log raw packet for debugging
		packetBytes := buf.Bytes()
		if len(packetBytes) >= 12 {
			flags := (uint16(packetBytes[2]) << 8) | uint16(packetBytes[3])
			qdcount := (uint16(packetBytes[4]) << 8) | uint16(packetBytes[5])
			ancount := (uint16(packetBytes[6]) << 8) | uint16(packetBytes[7])
			util.LogModuleInfo("dns", "[DNS-FWD] packet header: flags=0x%04x qdcount=%d ancount=%d", flags, qdcount, ancount)
		}
		
		// Parse response
		domain, ip, ttl := parseDNSResponseForCache(packetBytes)
		if domain == "" || ip == nil {
			util.LogModuleWarn("dns", "[DNS-FWD] failed to parse response: domain=%s ip=%v len=%d", domain, ip, len(packetBytes))
			continue
		}
		
		util.LogModuleInfo("dns", "[DNS-FWD] parsed response: domain=%s ip=%s ttl=%v", domain, ip, ttl)
		
		// 1. Cache the response
		h.cache.Set(domain, ip, ttl)

		// 2. Respond to the waiting client directly (no worker involved)
		key := strings.ToLower(domain)
		h.pendingMu.Lock()
		pq, ok := h.pendingQueries[key]
		if ok {
			delete(h.pendingQueries, key)
		}
		h.pendingMu.Unlock()

		if ok {
			resp := buildDNSResponse(pq.query, ip.To4())
			if resp != nil {
				if _, werr := h.udpEP.Write(&SlicePayload{Data: resp}, tcpip.WriteOptions{To: &pq.remoteAddr}); werr != nil {
					util.LogModuleWarn("dns", "[DNS-FWD] write response for %s to %s:%d fail: %v", domain, pq.remoteAddr.Addr, pq.remoteAddr.Port, werr)
				} else {
					util.LogModuleInfo("dns", "[DNS-FWD] responded to client: domain=%s ip=%s to %s:%d", domain, ip, pq.remoteAddr.Addr, pq.remoteAddr.Port)
				}
			}
		} else {
			util.LogModuleInfo("dns", "[DNS-FWD] no pending query for %s (cached only), pendingCount=%d", domain, len(h.pendingQueries))
		}
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
	// Note: parseDNSQueryDomain only works for queries (QR=0), so we need a separate function for responses
	domain, _, ok := parseDNSResponseDomain(resp)
	if !ok {
		return "", parsedIP, parsedTTL
	}
	
	return domain, parsedIP, parsedTTL
}

// parseDNSResponseDomain extracts the domain from a DNS response's question section.
// Unlike parseDNSQueryDomain, this works for responses (QR=1).
func parseDNSResponseDomain(pkt []byte) (string, uint16, bool) {
	if len(pkt) < 12 {
		return "", 0, false
	}
	
	// For responses, we don't check the QR bit - we just parse the question section
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
		// Handle compression pointers (0xC0 mask)
		if llen&0xc0 == 0xc0 {
			// Compression pointer - skip it and break
			off++
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
			// Matched a route (static or dynamic): forward without blocking —
			// the forwarder listener answers the client asynchronously.
			util.LogModuleInfo("dns", "[DNS-FWD-DEBUG] FORWARDING: domain=%s to subnet=%s", domain, remoteSubnet)
			h.forwardToRemoteAsync(remoteSubnet, packet, remoteAddr)
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

// forwardToRemoteAsync forwards a DNS query to the remote node's DNS hijacker
// without blocking the worker. The packet flows: netstack → writeLoop → mesh
// routing → remote HandleMeshFrame → GIP path → InjectMeshPacket → remote DNS
// hijacker; the response returns on the reverse path. The background listener
// responds directly to the client when the answer arrives, and sweepPending
// answers SERVFAIL on timeout. Late responses are cached for future queries.
func (h *DNSHijacker) forwardToRemoteAsync(remoteSubnet *net.IPNet, query []byte, remoteAddr tcpip.FullAddress) {
	// Derive remote GIP (.3) from subnet
	baseIP := remoteSubnet.IP.To4()
	if baseIP == nil {
		h.answerSERVFAIL(query, remoteAddr, "invalid remote subnet")
		return
	}
	remoteGIP := make(net.IP, 4)
	copy(remoteGIP, baseIP)
	remoteGIP[3] |= 3

	domain, _, _ := parseDNSQueryDomain(query)
	if domain == "" {
		h.answerSERVFAIL(query, remoteAddr, "no domain in query")
		return
	}
	key := strings.ToLower(domain)

	util.LogModuleInfo("dns", "[DNS-FWD] START: domain=%s remoteGIP=%s", domain, remoteGIP)

	// 1. Cache hit (possibly from a late response cached by the listener)
	if cachedIP := h.cache.Get(domain); cachedIP != nil {
		util.LogModuleInfo("dns", "[DNS-FWD] CACHE HIT: domain=%s ip=%s (late response cached)", domain, cachedIP)
		h.answerRemote(query, remoteAddr, cachedIP)
		return
	}

	// 2. Forwarder not initialized: a persistent-socket failure is systemic
	// (same netstack, same GIP bind the sync path would use) — fail loud.
	if h.forwarderSocket == nil {
		util.LogModuleError("dns", "[DNS-FWD] forwarder socket not initialized, cannot forward domain=%s", domain)
		h.answerSERVFAIL(query, remoteAddr, "forwarder not initialized")
		return
	}

	// 3. Register the waiting client. Concurrent queries for the same domain
	// share one upstream flight; the overwritten client is swept with SERVFAIL.
	queryCopy := make([]byte, len(query))
	copy(queryCopy, query)
	h.pendingMu.Lock()
	if _, exists := h.pendingQueries[key]; exists {
		util.LogModuleWarn("dns", "[DNS-FWD] duplicate pending for domain=%s, overwriting earlier client", domain)
	}
	h.pendingQueries[key] = &pendingQuery{query: queryCopy, remoteAddr: remoteAddr, at: time.Now()}
	pendingCount := len(h.pendingQueries)
	h.pendingMu.Unlock()

	util.LogModuleInfo("dns", "[DNS-FWD-DEBUG] registered pending for domain=%s pendingCount=%d", domain, pendingCount)

	// 4. Send query via persistent socket; the worker returns immediately.
	// The listener answers the client when the response arrives.
	remoteFwdAddr := tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFromSlice(remoteGIP), Port: 53}
	writeStart := time.Now()
	if _, err := h.forwarderSocket.Write(&SlicePayload{Data: queryCopy}, tcpip.WriteOptions{To: &remoteFwdAddr}); err != nil {
		util.LogModuleWarn("dns", "[DNS-FWD] WRITE FAILED: domain=%s err=%v", domain, err)
		h.pendingMu.Lock()
		delete(h.pendingQueries, key)
		h.pendingMu.Unlock()
		h.answerSERVFAIL(query, remoteAddr, fmt.Sprintf("write query: %v", err))
		return
	}
	util.LogModuleInfo("dns", "[DNS-FWD] WRITE OK: domain=%s remoteAddr=%s:%d duration=%v (worker released)", domain, remoteGIP, 53, time.Since(writeStart))
}

// answerRemote builds a DNS response for the resolved IP and sends it to the client.
func (h *DNSHijacker) answerRemote(query []byte, remoteAddr tcpip.FullAddress, ip net.IP) {
	resp := buildDNSResponse(query, ip.To4())
	if resp == nil {
		return
	}
	if _, err := h.udpEP.Write(&SlicePayload{Data: resp}, tcpip.WriteOptions{To: &remoteAddr}); err != nil {
		util.LogModuleWarn("dns", "tun dns: write remote response to %s:%d fail: %v", remoteAddr.Addr, remoteAddr.Port, err)
	}
}

// answerSERVFAIL sends SERVFAIL so the client retries promptly instead of
// waiting out its own (longer) resolver timeout.
func (h *DNSHijacker) answerSERVFAIL(query []byte, remoteAddr tcpip.FullAddress, reason string) {
	util.LogModuleWarn("dns", "[DNS-FWD] SERVFAIL to %s:%d: %s", remoteAddr.Addr, remoteAddr.Port, reason)
	resp := buildDNSErrorResponse(query)
	if resp == nil {
		return
	}
	if _, err := h.udpEP.Write(&SlicePayload{Data: resp}, tcpip.WriteOptions{To: &remoteAddr}); err != nil {
		util.LogModuleWarn("dns", "tun dns: write SERVFAIL to %s:%d fail: %v", remoteAddr.Addr, remoteAddr.Port, err)
	}
}

// sweepPending expires forwarded queries that never got a response and answers
// SERVFAIL to those clients. Runs on the forwarder listener's ticker.
func (h *DNSHijacker) sweepPending() {
	h.pendingMu.Lock()
	var expired []*pendingQuery
	for key, pq := range h.pendingQueries {
		if time.Since(pq.at) > forwardTimeout {
			expired = append(expired, pq)
			delete(h.pendingQueries, key)
		}
	}
	h.pendingMu.Unlock()

	for _, pq := range expired {
		domain, _, _ := parseDNSQueryDomain(pq.query)
		util.LogModuleWarn("dns", "[DNS-FWD] TIMEOUT: domain=%s no response in %v, answering SERVFAIL to %s:%d",
			domain, forwardTimeout, pq.remoteAddr.Addr, pq.remoteAddr.Port)
		resp := buildDNSErrorResponse(pq.query)
		if resp == nil {
			continue
		}
		if _, err := h.udpEP.Write(&SlicePayload{Data: resp}, tcpip.WriteOptions{To: &pq.remoteAddr}); err != nil {
			util.LogModuleWarn("dns", "tun dns: write SERVFAIL to %s:%d fail: %v", pq.remoteAddr.Addr, pq.remoteAddr.Port, err)
		}
	}
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
