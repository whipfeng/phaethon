package mesh

import (
	"net"
	"sync"
	"time"
)

// ModeBTable tracks Mode B (proxy entry) connections mapping destination address
// to the real client address. This allows the TUN forwarder to log the real
// client IP by looking up the destination.
type ModeBTable struct {
	mu      sync.RWMutex
	entries map[string]*ModeBEntry // key: "proto:dstIP:dstPort"
}

// ModeBEntry represents a single Mode B connection mapping.
type ModeBEntry struct {
	ClientAddr string // real client address (e.g., "192.168.1.100:12345")
	Inbound    string // entry protocol type (e.g., "SOCKS5:proxy1", "Trojan:xxx")
	CreatedAt  time.Time
}

// NewModeBTable creates a new Mode B connection tracking table.
func NewModeBTable() *ModeBTable {
	return &ModeBTable{
		entries: make(map[string]*ModeBEntry),
	}
}

// Register records a Mode B connection mapping by destination address.
// This should be called BEFORE dialing so the forwarder can look up the client.
// dstAddr is the destination being dialed (e.g., "10.161.88.10:30300").
// clientAddr is the real client's address.
// inbound is the entry protocol type (e.g., "SOCKS5:proxy1").
func (t *ModeBTable) Register(proto byte, dstAddr string, clientAddr string, inbound string) {
	if t == nil || dstAddr == "" {
		return
	}
	key := modeBDstKey(proto, dstAddr)
	t.mu.Lock()
	t.entries[key] = &ModeBEntry{
		ClientAddr: clientAddr,
		Inbound:    inbound,
		CreatedAt:  time.Now(),
	}
	t.mu.Unlock()
}

// Unregister removes a Mode B connection mapping by destination address.
func (t *ModeBTable) Unregister(proto byte, dstAddr string) {
	if t == nil || dstAddr == "" {
		return
	}
	key := modeBDstKey(proto, dstAddr)
	t.mu.Lock()
	delete(t.entries, key)
	t.mu.Unlock()
}

// LookupByDst returns the real client address and inbound type for a given destination.
// If no mapping exists, returns empty strings.
func (t *ModeBTable) LookupByDst(proto byte, dstIP net.IP, dstPort uint16) (clientAddr string, inbound string) {
	if t == nil {
		return "", ""
	}

	dstAddr := net.JoinHostPort(dstIP.String(), itoa(dstPort))
	key := modeBDstKey(proto, dstAddr)

	t.mu.RLock()
	entry, exists := t.entries[key]
	t.mu.RUnlock()

	if !exists {
		return "", ""
	}
	return entry.ClientAddr, entry.Inbound
}

func modeBDstKey(proto byte, dstAddr string) string {
	return string(rune(proto)) + ":dst:" + dstAddr
}
