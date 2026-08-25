package tun

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"sync"

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
		util.LogInfo("tun dns: %s -> %s", domain, fakeIP.String())

		resp := buildDNSResponse(packet, fakeIP.To4())
		if resp != nil {
			if _, err := h.udpEP.Write(&slicePayload{data: resp}, tcpip.WriteOptions{To: &res.RemoteAddr}); err != nil {
				util.LogWarn("tun dns: write response fail: %v", err)
			}
		}
	}
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
