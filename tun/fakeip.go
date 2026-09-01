package tun

import (
	"net"
	"sync"
)

// FakeIPPoolCIDR is the CIDR used for Fake-IP allocations.
const FakeIPPoolCIDR = "198.18.0.0/15"

// FakeIPPool manages the 198.18.0.0/15 fake IP allocation.
type FakeIPPool struct {
	mu         sync.RWMutex
	domainToIP map[string]net.IP
	ipToDomain map[string]string
	ipToRealIP map[string]net.IP // Fake-IP -> real IP cache
	reserved   map[uint32]bool
	nextIP     uint32
	onChange   func() // callback when pool stats change
}

// NewFakeIPPool creates a Fake-IP pool starting at 198.18.0.0.
// It reserves the network and broadcast addresses of 198.18.0.0/15.
func NewFakeIPPool() *FakeIPPool {
	reserved := map[uint32]bool{
		ipToUint32(net.ParseIP("198.18.0.0").To4()):     true, // network address
		ipToUint32(net.ParseIP("198.19.255.255").To4()): true, // broadcast address
	}
	return &FakeIPPool{
		domainToIP: make(map[string]net.IP),
		ipToDomain: make(map[string]string),
		ipToRealIP: make(map[string]net.IP),
		reserved:   reserved,
		nextIP:     ipToUint32(net.ParseIP("198.18.0.0").To4()),
	}
}

// SetOnChange sets a callback that is invoked when the pool stats change.
func (p *FakeIPPool) SetOnChange(fn func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onChange = fn
}

// Lookup returns a Fake-IP for the given domain, allocating if necessary.
func (p *FakeIPPool) Lookup(domain string) net.IP {
	p.mu.Lock()
	defer p.mu.Unlock()

	if ip, ok := p.domainToIP[domain]; ok {
		return ip
	}

	for {
		ip := uint32ToIP(p.nextIP)
		p.nextIP++
		if p.nextIP > ipToUint32(net.ParseIP("198.19.255.255").To4()) {
			p.nextIP = ipToUint32(net.ParseIP("198.18.0.0").To4())
		}
		if p.reserved[ipToUint32(ip)] {
			continue
		}

		// If this IP is already mapped to another domain (wrap-around collision),
		// evict the old mapping so the reverse lookup remains consistent.
		ipStr := ip.String()
		if oldDomain, exists := p.ipToDomain[ipStr]; exists {
			delete(p.domainToIP, oldDomain)
		}
		p.domainToIP[domain] = ip
		p.ipToDomain[ipStr] = domain
		if p.onChange != nil {
			p.onChange()
		}
		return ip
	}
}

// LookupDomain returns the original domain for a Fake-IP, or empty if not found.
func (p *FakeIPPool) LookupDomain(ip string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.ipToDomain[ip]
}

// SetRealIP caches the real IP address for a Fake-IP. This is called by the DNS
// hijacker after synchronously resolving the domain through the physical interface.
func (p *FakeIPPool) SetRealIP(fakeIP net.IP, realIP net.IP) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ipToRealIP[fakeIP.String()] = realIP
	if p.onChange != nil {
		p.onChange()
	}
}

// LookupRealIP returns the cached real IP for a Fake-IP, or nil if not found.
func (p *FakeIPPool) LookupRealIP(fakeIP net.IP) net.IP {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.ipToRealIP[fakeIP.String()]
}

// Release removes a domain's Fake-IP mapping.
func (p *FakeIPPool) Release(domain string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ip, ok := p.domainToIP[domain]; ok {
		ipStr := ip.String()
		delete(p.ipToDomain, ipStr)
		delete(p.domainToIP, domain)
		delete(p.ipToRealIP, ipStr)
		if p.onChange != nil {
			p.onChange()
		}
	}
}

// FakeIPStats contains snapshot statistics of the Fake-IP pool.
type FakeIPStats struct {
	DomainCount    int `json:"domainCount"`
	RealIPCacheCount int `json:"realIPCacheCount"`
}

// Stats returns a snapshot of the Fake-IP pool statistics.
func (p *FakeIPPool) Stats() FakeIPStats {
	p.mu.RLock()
	domainCount := len(p.domainToIP)
	realIPCacheCount := len(p.ipToRealIP)
	p.mu.RUnlock()

	return FakeIPStats{
		DomainCount:    domainCount,
		RealIPCacheCount: realIPCacheCount,
	}
}

func ipToUint32(ip net.IP) uint32 {
	ip = ip.To4()
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}

func uint32ToIP(n uint32) net.IP {
	return net.IPv4(byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
}
