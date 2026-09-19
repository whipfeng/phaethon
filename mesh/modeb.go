package mesh

import (
	"net"
	"sync"
	"time"

	"phaethon/config"
)

// ModeBTable tracks Mode B (proxy entry) connections mapping destination address
// and source port to the real client address. This allows the TUN forwarder to
// log the real client IP by looking up the destination and source port.
// Key format: "proto:dst:dstAddr:srcPort" to avoid collisions when multiple
// clients connect to the same destination.
type ModeBTable struct {
	mu      sync.RWMutex
	entries map[string]*ModeBEntry // key: "proto:dstIP:dstPort:srcPort"
}

// ModeBEntry represents a single Mode B connection mapping.
type ModeBEntry struct {
	ClientAddr string          // real client address (e.g., "192.168.1.100:12345")
	Inbound    string          // entry protocol type (e.g., "SOCKS5:proxy1", "Trojan:xxx")
	Mapping    *config.Mapping // original mapping for rule matching (nil for pure TUN)
	CreatedAt  time.Time
}

// NewModeBTable creates a new Mode B connection tracking table.
func NewModeBTable() *ModeBTable {
	return &ModeBTable{
		entries: make(map[string]*ModeBEntry),
	}
}

// Register records a Mode B connection mapping by destination address and source port.
// This should be called AFTER dialing so the source port (assigned by netstack) is known.
// dstAddr is the destination being dialed (e.g., "10.161.88.10:30300").
// srcPort is the source port assigned by the netstack (from conn.LocalAddr()).
// clientAddr is the real client's address.
// inbound is the entry protocol type (e.g., "SOCKS5:proxy1").
// mapping is the original *config.Mapping for rule matching (nil for pure TUN).
func (t *ModeBTable) Register(proto byte, dstAddr string, srcPort uint16, clientAddr string, inbound string, mapping *config.Mapping) {
	if t == nil || dstAddr == "" {
		return
	}
	key := modeBKey(proto, dstAddr, srcPort)
	t.mu.Lock()
	t.entries[key] = &ModeBEntry{
		ClientAddr: clientAddr,
		Inbound:    inbound,
		Mapping:    mapping,
		CreatedAt:  time.Now(),
	}
	t.mu.Unlock()
}

// Unregister removes a Mode B connection mapping by destination address and source port.
func (t *ModeBTable) Unregister(proto byte, dstAddr string, srcPort uint16) {
	if t == nil || dstAddr == "" {
		return
	}
	key := modeBKey(proto, dstAddr, srcPort)
	t.mu.Lock()
	delete(t.entries, key)
	t.mu.Unlock()
}

// LookupByDst returns the real client address, inbound type, and mapping for a given
// destination and source port. The srcPort is the source port seen by the gateway
// (from the mesh peer's connection).
// First tries exact match with srcPort, then falls back to srcPort=0 (placeholder registered before dial).
// If no mapping exists, returns empty strings and nil mapping.
func (t *ModeBTable) LookupByDst(proto byte, dstIP net.IP, dstPort uint16, srcPort uint16) (clientAddr string, inbound string, mapping *config.Mapping) {
	if t == nil {
		return "", "", nil
	}

	dstAddr := net.JoinHostPort(dstIP.String(), itoa(dstPort))

	// First try exact match with the specific srcPort
	key := modeBKey(proto, dstAddr, srcPort)
	t.mu.RLock()
	entry, exists := t.entries[key]
	t.mu.RUnlock()

	if exists {
		return entry.ClientAddr, entry.Inbound, entry.Mapping
	}

	// Fall back to srcPort=0 (placeholder registered before dial completed)
	if srcPort != 0 {
		key = modeBKey(proto, dstAddr, 0)
		t.mu.RLock()
		entry, exists = t.entries[key]
		t.mu.RUnlock()

		if exists {
			return entry.ClientAddr, entry.Inbound, entry.Mapping
		}
	}

	return "", "", nil
}

func modeBKey(proto byte, dstAddr string, srcPort uint16) string {
	return string(rune(proto)) + ":" + dstAddr + ":" + itoa(srcPort)
}
