package tun

import (
	"net"
	"sync"
)

// FakeIPPool manages the 198.18.0.0/15 fake IP allocation.
type FakeIPPool struct {
	mu         sync.RWMutex
	domainToIP map[string]net.IP
	ipToDomain map[string]string
	nextIP     uint32
}

// NewFakeIPPool creates a Fake-IP pool starting at 198.18.0.0.
func NewFakeIPPool() *FakeIPPool {
	return &FakeIPPool{
		domainToIP: make(map[string]net.IP),
		ipToDomain: make(map[string]string),
		nextIP:     ipToUint32(net.ParseIP("198.18.0.0").To4()),
	}
}

// Lookup returns a Fake-IP for the given domain, allocating if necessary.
func (p *FakeIPPool) Lookup(domain string) net.IP {
	p.mu.Lock()
	defer p.mu.Unlock()

	if ip, ok := p.domainToIP[domain]; ok {
		return ip
	}

	ip := uint32ToIP(p.nextIP)
	p.nextIP++
	if p.nextIP > ipToUint32(net.ParseIP("198.19.255.255").To4()) {
		p.nextIP = ipToUint32(net.ParseIP("198.18.0.0").To4())
	}

	// If this IP is already mapped to another domain (wrap-around collision),
	// evict the old mapping so the reverse lookup remains consistent.
	ipStr := ip.String()
	if oldDomain, exists := p.ipToDomain[ipStr]; exists {
		delete(p.domainToIP, oldDomain)
	}
	p.domainToIP[domain] = ip
	p.ipToDomain[ipStr] = domain
	return ip
}

// LookupDomain returns the original domain for a Fake-IP, or empty if not found.
func (p *FakeIPPool) LookupDomain(ip string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.ipToDomain[ip]
}

// Release removes a mapping.
func (p *FakeIPPool) Release(domain string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ip, ok := p.domainToIP[domain]; ok {
		delete(p.ipToDomain, ip.String())
		delete(p.domainToIP, domain)
	}
}

func ipToUint32(ip net.IP) uint32 {
	ip = ip.To4()
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}

func uint32ToIP(n uint32) net.IP {
	return net.IPv4(byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
}
