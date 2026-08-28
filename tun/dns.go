package tun

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"

	"phaethon/dialer"
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
}

// NewDNSHijacker creates a DNS hijacker bound to the netstack UDP stack.
func NewDNSHijacker(ns *stack.Stack, pool *FakeIPPool, tunAddr, dnsAddr tcpip.Address) *DNSHijacker {
	return &DNSHijacker{
		ns:      ns,
		pool:    pool,
		tunAddr: tunAddr,
		dnsAddr: dnsAddr,
	}
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
		NIC:  1,
		Addr: h.dnsAddr,
		Port: 53,
	}
	if err := h.udpEP.Bind(addr); err != nil {
		return fmt.Errorf("bind udp 53 on %s: %v", h.dnsAddr, err)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		h.serveLoop()
	}()
	return nil
}

// Stop closes the UDP endpoint.
func (h *DNSHijacker) Stop() {
	if h.started && h.udpEP != nil {
		h.udpEP.Close()
	}
}

// Resolve returns the raw DNS response bytes for a query without sending it
// through the netstack. It is used by the Windows-side DNS proxy and internal
// health probes so they do not depend on gVisor loopback delivery semantics.
// Before returning the Fake-IP, it synchronously resolves the real IP through
// the physical interface and caches it in the FakeIPPool, so the engine does
// not need to re-resolve the domain when handling the connection.
func (h *DNSHijacker) Resolve(query []byte) ([]byte, error) {
	if len(query) == 0 {
		return nil, fmt.Errorf("empty query")
	}
	domain, ok := parseDNSQueryDomain(query)
	if !ok || domain == "" {
		return nil, fmt.Errorf("failed to parse query")
	}
	fakeIP := h.pool.Lookup(domain)

	// Synchronously resolve the real IP through the physical interface.
	if realIP := h.resolveRealIP(domain); realIP != nil {
		h.pool.SetRealIP(fakeIP, realIP)
		util.LogDebug("tun dns: %s -> fake=%s real=%s", domain, fakeIP, realIP)
	}

	resp := buildDNSResponse(query, fakeIP.To4())
	if resp == nil {
		return nil, fmt.Errorf("failed to build response")
	}
	return resp, nil
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
				<-ch
				continue
			}
			util.LogWarn("tun dns: read error: %v", err)
			return
		}

		packet := buf.Bytes()
		if res.Total == 0 {
			continue
		}

		// Minimal DNS parsing: extract the queried domain
		domain, ok := parseDNSQueryDomain(packet)
		if !ok || domain == "" {
			util.LogWarn("tun dns: failed to parse query from %d bytes", len(packet))
			continue
		}

		fakeIP := h.pool.Lookup(domain)

		// Synchronously resolve the real IP through the physical interface.
		if realIP := h.resolveRealIP(domain); realIP != nil {
			h.pool.SetRealIP(fakeIP, realIP)
			util.LogDebug("tun dns: %s -> fake=%s real=%s", domain, fakeIP, realIP)
		}

		resp := buildDNSResponse(packet, fakeIP.To4())
		if resp == nil {
			continue
		}
		if _, err := h.udpEP.Write(&slicePayload{data: resp}, tcpip.WriteOptions{To: &res.RemoteAddr}); err != nil {
			util.LogWarn("tun dns: write response to %s:%d fail: %v", res.RemoteAddr.Addr, res.RemoteAddr.Port, err)
		} else {
			util.LogDebug("tun dns: %s -> %s", domain, fakeIP)
		}
	}
}

// resolveRealIP resolves the real IP for a domain through the physical interface,
// bypassing TUN split-tunnel routes. Returns nil if resolution fails.
func (h *DNSHijacker) resolveRealIP(domain string) net.IP {
	bc := dialer.GetGlobalBindContext()
	if bc == nil {
		// No BindContext yet (TUN still starting up); fall back to system DNS.
		ips, err := net.LookupIP(domain)
		if err != nil || len(ips) == 0 {
			return nil
		}
		for _, ip := range ips {
			if ip4 := ip.To4(); ip4 != nil {
				return ip4
			}
		}
		return nil
	}

	// Resolve through the original DNS servers via the physical interface.
	ips, err := dialer.ResolveRouteAware(domain)
	if err != nil || len(ips) == 0 {
		util.LogDebug("tun dns: real IP resolution failed for %s: %v", domain, err)
		return nil
	}
	return net.ParseIP(ips[0])
}

type slicePayload struct {
	data []byte
}

func (p *slicePayload) Len() int { return len(p.data) }
func (p *slicePayload) Read(dst []byte) (int, error) {
	n := copy(dst, p.data)
	p.data = p.data[n:]
	if len(p.data) == 0 {
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
	resp = append(resp, 0x00, 0x00, 0x00, 0x3c) // TTL 60
	resp = append(resp, 0x00, 0x04)             // RDLENGTH
	resp = append(resp, ip...)
	return resp
}

// buildDNSQuery builds a minimal DNS A query for domain using txID.
func buildDNSQuery(domain string, txID uint16) []byte {
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

// parseDNSResponseIP extracts the first A record IPv4 address from a DNS
// response. It returns nil if the response is invalid or not an A record.
func parseDNSResponseIP(resp []byte) net.IP {
	if len(resp) < 12 {
		return nil
	}
	flags := (uint16(resp[2]) << 8) | uint16(resp[3])
	if flags&0x8000 == 0 { // not a response
		return nil
	}
	if flags&0x000f != 0 { // RCODE != 0
		return nil
	}
	ancount := (uint16(resp[6]) << 8) | uint16(resp[7])
	if ancount == 0 {
		return nil
	}

	// Skip question section.
	off := 12
	for {
		if off >= len(resp) {
			return nil
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
			return nil
		}
		off += llen
	}
	off += 4 // QTYPE + QCLASS

	// Parse first answer.
	if off >= len(resp) {
		return nil
	}
	if resp[off]&0xc0 == 0xc0 {
		off += 2
	} else {
		for {
			if off >= len(resp) {
				return nil
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
				return nil
			}
			off += llen
		}
	}
	if off+10 > len(resp) {
		return nil
	}
	rtype := (uint16(resp[off]) << 8) | uint16(resp[off+1])
	rdlen := (uint16(resp[off+8]) << 8) | uint16(resp[off+9])
	off += 10
	if rtype != 0x0001 || rdlen != 4 {
		return nil
	}
	if off+4 > len(resp) {
		return nil
	}
	return net.IP(resp[off : off+4])
}

// isFakeIP reports whether the given IP string is in the Fake-IP range
// 198.18.0.0/15.
func isFakeIP(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	return ip4[0] == 198 && ip4[1] >= 18 && ip4[1] <= 19
}
