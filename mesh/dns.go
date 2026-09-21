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
		ns:         ns,
		pool:       pool,
		tunAddr:    tunAddr,
		dnsAddr:    dnsAddr,
		closeCh:    make(chan struct{}),
		cache:      NewDNSCache(),
		dnsQueryCh: make(chan dnsQuery, 1024),
	}
}

// BindNetstack sets the netstack and addresses for the DNS hijacker.
// Called when TUN engine is initialized and can provide the netstack.
func (h *DNSHijacker) BindNetstack(ns *stack.Stack, tunAddr, dnsAddr tcpip.Address) {
	h.ns = ns
	h.tunAddr = tunAddr
	h.dnsAddr = dnsAddr
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
	domain, ok := parseDNSQueryDomain(query)
	if !ok || domain == "" {
		return nil, fmt.Errorf("failed to parse query")
	}

	fakeIP := h.pool.Lookup(domain)
	util.LogDebug("tun dns: %s -> fake=%s", domain, fakeIP)

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
			util.LogWarn("tun dns: read error: %v", err)
			return
		}

		packet := buf.Bytes()
		if res.Total == 0 {
			continue
		}

		// Minimal DNS parsing: extract the queried domain for logging
		domain, ok := parseDNSQueryDomain(packet)
		if ok && domain != "" {
			srcIP := net.IP(res.RemoteAddr.Addr.AsSlice())
			srcPort := res.RemoteAddr.Port
			util.LogDebug("[DNS] DNSHijacker: query domain=%s from=%s:%d", domain, srcIP, srcPort)
		}

		// Queue for async processing by worker pool
		packetCopy := make([]byte, len(packet))
		copy(packetCopy, packet)
		select {
		case h.dnsQueryCh <- dnsQuery{packet: packetCopy, remoteAddr: res.RemoteAddr}:
			// Queued successfully
		default:
			util.LogDebug("[DNS] dnsQueryCh full, dropping query")
		}
	}
}

// processQuery handles a single DNS query: cache check, local pool, or remote forward.
func (h *DNSHijacker) processQuery(packet []byte, remoteAddr tcpip.FullAddress) {
	domain, ok := parseDNSQueryDomain(packet)
	if !ok || domain == "" {
		util.LogWarn("tun dns: failed to parse query from %d bytes", len(packet))
		return
	}

	// Check cache first
	if cachedIP := h.cache.Get(domain); cachedIP != nil {
		util.LogDebug("tun dns: %s -> %s (cached)", domain, cachedIP)
		resp := buildDNSResponse(packet, cachedIP.To4())
		if resp != nil {
			if _, err := h.udpEP.Write(&SlicePayload{Data: resp}, tcpip.WriteOptions{To: &remoteAddr}); err != nil {
				util.LogWarn("tun dns: write cached response for %s to %s:%d fail: %v", domain, remoteAddr.Addr, remoteAddr.Port, err)
			}
		}
		return
	}

	// Check if domain belongs to a remote node
	if h.resolveDomainSubnet != nil {
		remoteSubnet, needsFail := h.resolveDomainSubnet(domain)
		if remoteSubnet != nil {
			// Matched a route (static or dynamic), forward to remote
			remoteIP, ttl, err := h.forwardToRemote(remoteSubnet, packet)
			if err != nil {
				util.LogWarn("tun dns: forward %s to remote failed: %v", domain, err)
				// Return SERVFAIL instead of fallback to local pool
				// This allows the client to retry and get the correct IP once mesh recovers
				resp := buildDNSErrorResponse(packet)
				if resp != nil {
					if _, err := h.udpEP.Write(&SlicePayload{Data: resp}, tcpip.WriteOptions{To: &remoteAddr}); err != nil {
						util.LogWarn("tun dns: write SERVFAIL for %s to %s:%d fail: %v", domain, remoteAddr.Addr, remoteAddr.Port, err)
					}
				}
				return
			}
			h.cache.Set(domain, remoteIP, ttl)
			util.LogDebug("tun dns: %s -> %s (remote, ttl=%v, cached)", domain, remoteIP, ttl)
			resp := buildDNSResponse(packet, remoteIP.To4())
			if resp != nil {
				if _, err := h.udpEP.Write(&SlicePayload{Data: resp}, tcpip.WriteOptions{To: &remoteAddr}); err != nil {
					util.LogWarn("tun dns: write remote response for %s to %s:%d fail: %v", domain, remoteAddr.Addr, remoteAddr.Port, err)
				}
			}
			return
		} else if needsFail {
			// Static match but node not ready → SERVFAIL
			util.LogWarn("tun dns: %s matched static route but node not ready, returning SERVFAIL", domain)
			resp := buildDNSErrorResponse(packet)
			if resp != nil {
				if _, err := h.udpEP.Write(&SlicePayload{Data: resp}, tcpip.WriteOptions{To: &remoteAddr}); err != nil {
					util.LogWarn("tun dns: write SERVFAIL for %s to %s:%d fail: %v", domain, remoteAddr.Addr, remoteAddr.Port, err)
				}
			}
			return
		}
		// else: no match, fallback to local pool
	}

	// Local pool resolution (default)
	fakeIP := h.pool.Lookup(domain)
	util.LogDebug("tun dns: %s -> %s", domain, fakeIP)
	resp := buildDNSResponse(packet, fakeIP.To4())

	if resp == nil {
		return
	}
	if _, err := h.udpEP.Write(&SlicePayload{Data: resp}, tcpip.WriteOptions{To: &remoteAddr}); err != nil {
		util.LogWarn("tun dns: write response to %s:%d fail: %v", remoteAddr.Addr, remoteAddr.Port, err)
	} else {
		util.LogDebug("tun dns: %s -> response sent (%d bytes)", domain, len(resp))
	}
}

// forwardToRemote sends a DNS query to the remote node's DNS hijacker via a
// gvisor netstack UDP socket. The packet flows: netstack → writeLoop → mesh
// routing → remote HandleMeshFrame → GIP path → InjectMeshPacket → remote DNS
// hijacker. The response follows the reverse path.
func (h *DNSHijacker) forwardToRemote(remoteSubnet *net.IPNet, query []byte) (net.IP, time.Duration, error) {
	// Derive remote GIP (.3) from subnet
	baseIP := remoteSubnet.IP.To4()
	if baseIP == nil {
		return nil, 0, fmt.Errorf("invalid remote subnet")
	}
	remoteGIP := make(net.IP, 4)
	copy(remoteGIP, baseIP)
	remoteGIP[3] |= 3

	util.LogDebug("[DNS] forwardToRemote: remoteGIP=%s subnet=%s", remoteGIP, remoteSubnet)

	remoteAddr := tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFromSlice(remoteGIP), Port: 53}
	conn, err := gonet.DialUDP(h.ns, nil, &remoteAddr, ipv4.ProtocolNumber)
	if err != nil {
		return nil, 0, fmt.Errorf("dial %s:53: %v", remoteGIP, err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
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
	util.LogDebug("[DNS] forwardToRemote: got response Fake-IP=%s ttl=%v", respIP, ttl)
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

// parseDNSQueryDomain extracts the queried domain from a DNS query packet.
func parseDNSQueryDomain(pkt []byte) (string, bool) {
	if len(pkt) < 12 {
		return "", false
	}
	flags := (uint16(pkt[2]) << 8) | uint16(pkt[3])
	if flags&0x8000 != 0 {
		return "", false
	}
	qdcount := (uint16(pkt[4]) << 8) | uint16(pkt[5])
	if qdcount == 0 {
		return "", false
	}

	off := 12
	var labels []string
	for {
		if off >= len(pkt) {
			return "", false
		}
		llen := int(pkt[off])
		off++
		if llen == 0 {
			break
		}
		if llen > 63 || off+llen > len(pkt) {
			return "", false
		}
		labels = append(labels, string(pkt[off:off+llen]))
		off += llen
	}

	var domain string
	for i, l := range labels {
		if i > 0 {
			domain += "."
		}
		domain += l
	}
	return domain, true
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
