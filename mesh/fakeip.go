package mesh

import (
	"net"
	"sync"

	"phaethon/db"
	"phaethon/util"
)

// FakeIPPool manages fake IP allocation.
type FakeIPPool struct {
	mu         sync.RWMutex
	domainToIP map[string]net.IP
	ipToDomain map[string]string
	ipToRealIP map[string]net.IP // Fake-IP -> real IP cache
	reserved   map[uint32]bool
	nextIP     uint32
	poolStart  uint32 // first IP in pool range
	poolEnd    uint32 // last IP in pool range
	onChange   func() // callback when pool stats change
}

// NewFakeIPPoolWithSubnet creates a Fake-IP pool from a custom subnet.
// The first `skip` addresses in the subnet are reserved (e.g., VIP, hostIP, GIP).
// Used in mesh mode where each node allocates fakeIPs from its /20 subnet.
func NewFakeIPPoolWithSubnet(subnet *net.IPNet, skip int) *FakeIPPool {
	ip4 := subnet.IP.To4()
	start := ipToUint32(ip4)
	ones, bits := subnet.Mask.Size()
	hostBits := bits - ones
	total := uint32(1) << uint(hostBits)
	end := start + total - 1

	reserved := map[uint32]bool{
		start: true, // network address
		end:   true, // broadcast address
	}
	for i := uint32(0); i <= uint32(skip); i++ {
		reserved[start+i] = true
	}

	firstAlloc := start + uint32(skip) + 1

	return &FakeIPPool{
		domainToIP: make(map[string]net.IP),
		ipToDomain: make(map[string]string),
		ipToRealIP: make(map[string]net.IP),
		reserved:   reserved,
		nextIP:     firstAlloc,
		poolStart:  start,
		poolEnd:    end,
	}
}

// SetOnChange sets a callback that is invoked when the pool stats change.
func (p *FakeIPPool) SetOnChange(fn func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onChange = fn
}

// LoadFromDB loads existing FakeIP mappings from the database.
// Called during startup to restore persistent mappings.
func (p *FakeIPPool) LoadFromDB() {
	if !db.IsInitialized() {
		return
	}

	mappings, err := db.GetAllFakeIPs()
	if err != nil {
		util.LogWarn("Failed to load FakeIP mappings from db: %v", err)
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	loaded := 0
	for _, entry := range mappings {
		ip := net.ParseIP(entry.IP)
		if ip == nil {
			continue
		}

		// Skip if IP is outside our pool range
		if !p.Contains(ip) {
			continue
		}

		ipStr := ip.String()
		p.domainToIP[entry.Domain] = ip
		p.ipToDomain[ipStr] = entry.Domain

		// Update nextIP to avoid collision
		ipNum := ipToUint32(ip)
		if ipNum >= p.nextIP {
			p.nextIP = ipNum + 1
			if p.nextIP > p.poolEnd {
				p.nextIP = p.poolStart
			}
		}

		loaded++
	}

	if loaded > 0 {
		util.LogInfo("Loaded %d FakeIP mappings from db", loaded)
	}
}

// Lookup returns a Fake-IP for the given domain, allocating if necessary.
func (p *FakeIPPool) Lookup(domain string) net.IP {
	p.mu.Lock()
	defer p.mu.Unlock()

	if ip, ok := p.domainToIP[domain]; ok {
		util.LogInfo("[FAKEIP] Lookup cache hit: %s -> %s", domain, ip)
		return ip
	}

	for {
		ip := uint32ToIP(p.nextIP)
		p.nextIP++
		if p.nextIP > p.poolEnd {
			p.nextIP = p.poolStart
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

		util.LogInfo("[FAKEIP] Lookup allocated: %s -> %s (pool nextIP=%d)", domain, ip, p.nextIP)

		// Persist to database
		if db.IsInitialized() {
			if err := db.PutFakeIP(domain, ipStr); err != nil {
				util.LogWarn("[FAKEIP] Failed to persist mapping: %v", err)
			}
		}

		if p.onChange != nil {
			p.onChange()
		}
		return ip
	}
}

// LookupDomain returns the original domain for a Fake-IP, or empty if not found.
func (p *FakeIPPool) LookupDomain(ip string) string {
	// Copy the map under the lock to avoid holding the lock during the lookup
	// This prevents potential deadlocks if the caller holds other locks
	p.mu.RLock()
	ipToDomainCopy := make(map[string]string, len(p.ipToDomain))
	for k, v := range p.ipToDomain {
		ipToDomainCopy[k] = v
	}
	p.mu.RUnlock()
	
	return ipToDomainCopy[ip]
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

		// Delete from database
		if db.IsInitialized() {
			if err := db.DeleteFakeIP(domain, ipStr); err != nil {
				util.LogWarn("Failed to delete FakeIP mapping from db: %v", err)
			}
		}

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

// Contains reports whether the given IP is within this pool's range.
func (p *FakeIPPool) Contains(ip net.IP) bool {
	n := ipToUint32(ip.To4())
	return n >= p.poolStart && n <= p.poolEnd
}

// InAllocRange reports whether ip falls inside the allocatable fake-IP range:
// within [poolStart, poolEnd] but not one of the reserved infrastructure
// addresses (network/broadcast and the first skip addresses).
func (p *FakeIPPool) InAllocRange(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	n := ipToUint32(ip4)
	if n < p.poolStart || n > p.poolEnd {
		return false
	}
	return !p.reserved[n]
}

func ipToUint32(ip net.IP) uint32 {
	ip = ip.To4()
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}

func uint32ToIP(n uint32) net.IP {
	return net.IPv4(byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
}
