package mesh

import (
	"net"
	"sync"
	"time"
)

// ModeBTable tracks Mode B (proxy entry) connections mapping netstack socket
// local address (GIP:port) to the real client address.
// This allows the TUN forwarder to log the real client IP instead of GIP.
type ModeBTable struct {
	mu      sync.RWMutex
	entries map[string]*ModeBEntry // key: "proto:GIP:port"
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

// Register records a Mode B connection mapping.
// localAddr is the netstack socket's local address (GIP:port).
// clientAddr is the real client's address.
// inbound is the entry protocol type (e.g., "SOCKS5:proxy1").
func (t *ModeBTable) Register(proto byte, localAddr net.Addr, clientAddr string, inbound string) {
	if t == nil || localAddr == nil {
		return
	}
	key := modeBKey(proto, localAddr)
	t.mu.Lock()
	t.entries[key] = &ModeBEntry{
		ClientAddr: clientAddr,
		Inbound:    inbound,
		CreatedAt:  time.Now(),
	}
	t.mu.Unlock()
}

// Unregister removes a Mode B connection mapping.
func (t *ModeBTable) Unregister(proto byte, localAddr net.Addr) {
	if t == nil || localAddr == nil {
		return
	}
	key := modeBKey(proto, localAddr)
	t.mu.Lock()
	delete(t.entries, key)
	t.mu.Unlock()
}

// Lookup returns the real client address and inbound type for a given source address.
// If srcIP is not a local GIP or no mapping exists, returns empty strings.
func (t *ModeBTable) Lookup(proto byte, srcIP net.IP, srcPort uint16, localGIPs []net.IP) (clientAddr string, inbound string) {
	if t == nil {
		return "", ""
	}

	// Check if srcIP is a local GIP
	isLocalGIP := false
	for _, gip := range localGIPs {
		if srcIP.Equal(gip) {
			isLocalGIP = true
			break
		}
	}
	if !isLocalGIP {
		return "", ""
	}

	// Build key and lookup
	tcpAddr := &net.TCPAddr{IP: srcIP, Port: int(srcPort)}
	key := modeBKey(proto, tcpAddr)

	t.mu.RLock()
	entry, exists := t.entries[key]
	t.mu.RUnlock()

	if !exists {
		return "", ""
	}
	return entry.ClientAddr, entry.Inbound
}

func modeBKey(proto byte, addr net.Addr) string {
	switch a := addr.(type) {
	case *net.TCPAddr:
		return string(rune(proto)) + ":tcp:" + a.IP.String() + ":" + itoa(uint16(a.Port))
	case *net.UDPAddr:
		return string(rune(proto)) + ":udp:" + a.IP.String() + ":" + itoa(uint16(a.Port))
	default:
		return string(rune(proto)) + ":" + addr.String()
	}
}
