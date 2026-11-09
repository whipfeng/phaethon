package mesh

import (
	"container/list"
	"net"
	"sync"

	"phaethon/db"
	"phaethon/util"
)

// FakeIPPool manages fake IP allocation.
//
// Allocation policy (D28): addresses are handed out in ascending order from the
// allocatable range; when the pool is exhausted the least-recently-used mapping
// is evicted (doubly-linked list, O(1) touch/evict) and its address reused.
// Database persistence happens on a background writer so a slow bolt write
// never blocks DNS resolution, and onChange runs outside the pool lock.
type FakeIPPool struct {
	mu         sync.RWMutex
	domainToIP map[string]net.IP
	ipToDomain map[string]string
	ipToRealIP map[string]net.IP // Fake-IP -> real IP cache
	lru        *list.List        // front = least recently used
	lruItems   map[string]*list.Element
	free       []uint32 // allocatable addresses, ascending
	freeHead   int      // next unused element of free
	reserved   map[uint32]bool
	poolStart  uint32 // first IP in pool range
	poolEnd    uint32 // last IP in pool range
	onChange   func() // callback when pool stats change
	disabled   bool   // pool misconfigured (no allocatable addresses)

	persistOnce sync.Once
	persistCh   chan fakeIPPersistOp
}

// fakeIPPersistOp is one queued database mutation.
type fakeIPPersistOp struct {
	put    bool
	domain string
	ip     string
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

	// D28: capacity is validated once at construction — an empty allocatable
	// range is a configuration error, not something to paper over at runtime.
	free := make([]uint32, 0, total)
	for n := start; n <= end; n++ {
		if reserved[n] {
			continue
		}
		free = append(free, n)
	}

	p := &FakeIPPool{
		domainToIP: make(map[string]net.IP),
		ipToDomain: make(map[string]string),
		ipToRealIP: make(map[string]net.IP),
		lru:        list.New(),
		lruItems:   make(map[string]*list.Element),
		free:       free,
		reserved:   reserved,
		poolStart:  start,
		poolEnd:    end,
	}
	if len(free) == 0 {
		p.disabled = true
		util.LogModuleError("dns", "[FAKEIP] pool %s has no allocatable address (subnet=%s skip=%d): fake-IP disabled",
			subnet.String(), subnet.String(), skip)
	}
	return p
}

// SetOnChange sets a callback that is invoked when the pool stats change.
func (p *FakeIPPool) SetOnChange(fn func()) {
	p.mu.Lock()
	p.onChange = fn
	p.mu.Unlock()
}

// LoadFromDB loads existing FakeIP mappings from the database.
// Called during startup to restore persistent mappings.
func (p *FakeIPPool) LoadFromDB() {
	if !db.IsInitialized() {
		return
	}

	mappings, err := db.GetAllFakeIPs()
	if err != nil {
		util.LogModuleWarn("dns", "Failed to load FakeIP mappings from db: %v", err)
		return
	}

	p.mu.Lock()
	loaded := 0
	for _, entry := range mappings {
		ip := net.ParseIP(entry.IP)
		if ip == nil || !p.Contains(ip) {
			continue
		}
		ipStr := ip.String()
		p.domainToIP[entry.Domain] = ip
		p.ipToDomain[ipStr] = entry.Domain
		p.lruItems[entry.Domain] = p.lru.PushBack(entry.Domain)

		// Take the address out of the free list (ascending scan is fine: this
		// runs once at startup and restores at most one full pool).
		target := ipToUint32(ip)
		for i := p.freeHead; i < len(p.free); i++ {
			if p.free[i] == target {
				p.free[i] = p.free[p.freeHead]
				p.free[p.freeHead] = target
				p.freeHead++
				break
			}
		}
		loaded++
	}
	onChange := p.onChange
	p.mu.Unlock()

	if loaded > 0 {
		util.LogModuleInfo("dns", "Loaded %d FakeIP mappings from db", loaded)
	}
	if onChange != nil && loaded > 0 {
		onChange()
	}
}

// Lookup returns a Fake-IP for the given domain, allocating if necessary.
// Returns nil only when the pool was misconfigured (no allocatable addresses).
func (p *FakeIPPool) Lookup(domain string) net.IP {
	p.mu.Lock()
	if p.disabled {
		p.mu.Unlock()
		return nil
	}
	if ip, ok := p.domainToIP[domain]; ok {
		if el, ok2 := p.lruItems[domain]; ok2 {
			p.lru.MoveToBack(el) // mark most recently used
		}
		p.mu.Unlock()
		return ip
	}

	var ipNum uint32
	var evicted *fakeIPPersistOp
	switch {
	case p.freeHead < len(p.free):
		ipNum = p.free[p.freeHead]
		p.freeHead++
	default:
		// Pool exhausted: evict the least recently used mapping and reuse its
		// address (D28: LRU, O(1)).
		front := p.lru.Front()
		if front == nil {
			p.mu.Unlock()
			util.LogModuleWarn("dns", "[FAKEIP] pool exhausted with no mapping to evict (domain=%s)", domain)
			return nil
		}
		oldDomain := front.Value.(string)
		oldIP := p.domainToIP[oldDomain]
		oldIPStr := oldIP.String()
		p.lru.Remove(front)
		delete(p.lruItems, oldDomain)
		delete(p.domainToIP, oldDomain)
		delete(p.ipToDomain, oldIPStr)
		delete(p.ipToRealIP, oldIPStr)
		ipNum = ipToUint32(oldIP)
		evicted = &fakeIPPersistOp{put: false, domain: oldDomain, ip: oldIPStr}
	}

	ip := uint32ToIP(ipNum)
	ipStr := ip.String()
	p.domainToIP[domain] = ip
	p.ipToDomain[ipStr] = domain
	p.lruItems[domain] = p.lru.PushBack(domain)
	onChange := p.onChange
	p.mu.Unlock()

	if evicted != nil {
		p.persist(*evicted)
	}
	p.persist(fakeIPPersistOp{put: true, domain: domain, ip: ipStr})
	if onChange != nil {
		onChange()
	}
	util.LogModuleDebug("dns", "[FAKEIP] allocated: %s -> %s", domain, ip)
	return ip
}

// LookupDomain returns the original domain for a Fake-IP, or empty if not found.
func (p *FakeIPPool) LookupDomain(ip string) string {
	p.mu.RLock()
	result := p.ipToDomain[ip]
	p.mu.RUnlock()
	if result == "" {
		util.LogModuleDebug("dns", "[FAKEIP] LookupDomain: %s -> (not found)", ip)
	}
	return result
}

// SetRealIP caches the real IP address for a Fake-IP. This is called by the DNS
// hijacker after synchronously resolving the domain through the physical interface.
func (p *FakeIPPool) SetRealIP(fakeIP net.IP, realIP net.IP) {
	p.mu.Lock()
	p.ipToRealIP[fakeIP.String()] = realIP
	onChange := p.onChange
	p.mu.Unlock()
	if onChange != nil {
		onChange()
	}
}

// LookupRealIP returns the cached real IP for a Fake-IP, or nil if not found.
func (p *FakeIPPool) LookupRealIP(fakeIP net.IP) net.IP {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.ipToRealIP[fakeIP.String()]
}

// Release removes a domain's Fake-IP mapping and returns the address to the
// free list.
func (p *FakeIPPool) Release(domain string) {
	p.mu.Lock()
	ip, ok := p.domainToIP[domain]
	if !ok {
		p.mu.Unlock()
		return
	}
	ipStr := ip.String()
	if el, ok2 := p.lruItems[domain]; ok2 {
		p.lru.Remove(el)
		delete(p.lruItems, domain)
	}
	delete(p.ipToDomain, ipStr)
	delete(p.domainToIP, domain)
	delete(p.ipToRealIP, ipStr)
	p.free = append(p.free, ipToUint32(ip))
	onChange := p.onChange
	p.mu.Unlock()

	p.persist(fakeIPPersistOp{put: false, domain: domain, ip: ipStr})
	if onChange != nil {
		onChange()
	}
}

// persist queues a database mutation. Writes happen on a background goroutine so
// a slow bolt transaction cannot stall DNS resolution (D28); if the queue is
// full the write falls back to synchronous so state is never silently lost.
func (p *FakeIPPool) persist(op fakeIPPersistOp) {
	if !db.IsInitialized() {
		return
	}
	p.persistOnce.Do(func() {
		p.persistCh = make(chan fakeIPPersistOp, 256)
		go func() {
			for queued := range p.persistCh {
				applyFakeIPOp(queued)
			}
		}()
	})
	select {
	case p.persistCh <- op:
	default:
		applyFakeIPOp(op)
	}
}

func applyFakeIPOp(op fakeIPPersistOp) {
	if op.put {
		if err := db.PutFakeIP(op.domain, op.ip); err != nil {
			util.LogModuleWarn("dns", "[FAKEIP] failed to persist %s -> %s: %v", op.domain, op.ip, err)
		}
		return
	}
	if err := db.DeleteFakeIP(op.domain, op.ip); err != nil {
		util.LogModuleWarn("dns", "[FAKEIP] failed to delete %s -> %s: %v", op.domain, op.ip, err)
	}
}

// FakeIPStats contains snapshot statistics of the Fake-IP pool.
type FakeIPStats struct {
	DomainCount      int `json:"domainCount"`
	RealIPCacheCount int `json:"realIPCacheCount"`
}

// Stats returns a snapshot of the Fake-IP pool statistics.
func (p *FakeIPPool) Stats() FakeIPStats {
	p.mu.RLock()
	domainCount := len(p.domainToIP)
	realIPCacheCount := len(p.ipToRealIP)
	p.mu.RUnlock()

	return FakeIPStats{
		DomainCount:      domainCount,
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
