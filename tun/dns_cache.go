package tun

import (
	"sync"
	"time"
)

// dnsCacheEntry stores resolved addresses and their expiration time.
type dnsCacheEntry struct {
	ips       []string
	expiresAt time.Time
}

// DNSCache is a thread-safe in-memory cache for TUN-internal DNS resolution.
// It avoids repeated upstream queries for the same domain and provides a short
// negative-cache window for failed lookups.
type DNSCache struct {
	mu      sync.RWMutex
	entries map[string]*dnsCacheEntry
	maxTTL  time.Duration
	negTTL  time.Duration
}

// NewDNSCache creates a DNS cache with sensible TTL limits.
func NewDNSCache() *DNSCache {
	return &DNSCache{
		entries: make(map[string]*dnsCacheEntry),
		maxTTL:  5 * time.Minute,
		negTTL:  5 * time.Second,
	}
}

// key builds the cache key from domain and query type.
func key(domain, qtype string) string {
	return qtype + ":" + domain
}

// Get returns cached IPs if present and not expired. Returns nil on miss,
// expiration, or negative cache entry.
func (c *DNSCache) Get(domain, qtype string) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	ent, ok := c.entries[key(domain, qtype)]
	if !ok || time.Now().After(ent.expiresAt) || len(ent.ips) == 0 {
		return nil
	}
	out := make([]string, len(ent.ips))
	copy(out, ent.ips)
	return out
}

// Set stores resolved IPs with the given TTL, capped by maxTTL.
func (c *DNSCache) Set(domain, qtype string, ips []string, ttl time.Duration) {
	if ttl <= 0 || ttl > c.maxTTL {
		ttl = c.maxTTL
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make([]string, len(ips))
	copy(out, ips)
	c.entries[key(domain, qtype)] = &dnsCacheEntry{
		ips:       out,
		expiresAt: time.Now().Add(ttl),
	}
}

// SetNegative stores a negative cache entry for a failed lookup.
func (c *DNSCache) SetNegative(domain, qtype string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key(domain, qtype)] = &dnsCacheEntry{
		ips:       nil,
		expiresAt: time.Now().Add(c.negTTL),
	}
}

// Clean removes expired entries. It is safe for concurrent use.
func (c *DNSCache) Clean() {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, ent := range c.entries {
		if now.After(ent.expiresAt) {
			delete(c.entries, k)
		}
	}
}
