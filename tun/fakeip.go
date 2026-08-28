package tun

import (
	"net"
	"sync"

	"phaethon/util"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
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

	// ns is the netstack used to register each allocated Fake-IP as a local
	// address. This makes the TCP/UDP forwarders fire reliably for Fake-IP
	// destinations instead of relying on short-lived promiscuous addresses.
	ns         *stack.Stack
	nicID      tcpip.NICID
	registered map[string]bool
	regMu      sync.Mutex
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
		registered: make(map[string]bool),
	}
}

// NewFakeIPPoolWithStack creates a Fake-IP pool that registers each allocated
// IP as a local netstack address on the given NIC.
func NewFakeIPPoolWithStack(ns *stack.Stack, nicID tcpip.NICID) *FakeIPPool {
	p := NewFakeIPPool()
	p.ns = ns
	p.nicID = nicID
	return p
}

// Lookup returns a Fake-IP for the given domain, allocating if necessary.
func (p *FakeIPPool) Lookup(domain string) net.IP {
	p.mu.Lock()
	defer p.mu.Unlock()

	if ip, ok := p.domainToIP[domain]; ok {
		p.registerIPLocked(ip)
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
		p.registerIPLocked(ip)
		return ip
	}
}

// registerIPLocked ensures the allocated Fake-IP is known to netstack as a
// local address. The pool write lock must be held.
func (p *FakeIPPool) registerIPLocked(ip net.IP) {
	if p.ns == nil {
		return
	}
	ip = ip.To4()
	if ip == nil {
		return
	}
	ipStr := ip.String()
	p.regMu.Lock()
	if p.registered[ipStr] {
		p.regMu.Unlock()
		return
	}
	p.regMu.Unlock()

	addr := tcpip.AddrFrom4([4]byte(ip))
	protoAddr := tcpip.ProtocolAddress{
		Protocol: ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{
			Address:   addr,
			PrefixLen: 32,
		},
	}
	if err := p.ns.AddProtocolAddress(p.nicID, protoAddr, stack.AddressProperties{}); err != nil {
		util.LogWarn("tun fakeip: add address %s fail: %v", ipStr, err)
		return
	}
	util.LogInfo("tun fakeip: registered local address %s", ipStr)

	p.regMu.Lock()
	p.registered[ipStr] = true
	p.regMu.Unlock()
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
}

// LookupRealIP returns the cached real IP for a Fake-IP, or nil if not found.
func (p *FakeIPPool) LookupRealIP(fakeIP net.IP) net.IP {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.ipToRealIP[fakeIP.String()]
}

// Release removes a mapping and unregisters the Fake-IP from netstack.
func (p *FakeIPPool) Release(domain string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ip, ok := p.domainToIP[domain]; ok {
		ipStr := ip.String()
		delete(p.ipToDomain, ipStr)
		delete(p.domainToIP, domain)
		delete(p.ipToRealIP, ipStr)

		// Unregister the Fake-IP from netstack so it doesn't accumulate as a local address.
		if p.ns != nil {
			p.regMu.Lock()
			if p.registered[ipStr] {
				addr := tcpip.AddrFrom4([4]byte(ip.To4()))
				if err := p.ns.RemoveAddress(p.nicID, addr); err != nil {
					util.LogWarn("tun fakeip: remove address %s fail: %v", ipStr, err)
				} else {
					util.LogInfo("tun fakeip: unregistered local address %s", ipStr)
					delete(p.registered, ipStr)
				}
			}
			p.regMu.Unlock()
		}
	}
}

func ipToUint32(ip net.IP) uint32 {
	ip = ip.To4()
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}

func uint32ToIP(n uint32) net.IP {
	return net.IPv4(byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
}
