package mesh

import (
	"net"
	"strconv"
	"sync"
	"time"
)

// TUNSrcTable records the original source IP of TUN-entered packets BEFORE
// the gVisor Input-chain SNAT rewrites the source to the local VIP. The TUN
// read loop registers the first packet of each flow; the netstack forwarder
// resolves the display source from this table via ResolveOriginalSrc.
//
// The table serves display only: a lookup miss degrades to showing the VIP,
// never affects forwarding (which is driven by conntrack, not this table).
// Modeled after ModeBTable, but TUN flows have no explicit teardown signal,
// so entries expire after a TTL well beyond the first-packet→accept window.
type TUNSrcTable struct {
	mu      sync.RWMutex
	entries map[string]tunSrcEntry
	done    chan struct{}
	wg      sync.WaitGroup
}

type tunSrcEntry struct {
	origSrc   net.IP
	createdAt time.Time
}

const (
	tunSrcTTL        = 10 * time.Minute
	tunSrcSweepEvery = 5 * time.Minute
)

// NewTUNSrcTable creates a TUN source table and starts its expiry sweeper.
func NewTUNSrcTable() *TUNSrcTable {
	t := &TUNSrcTable{
		entries: make(map[string]tunSrcEntry),
		done:    make(chan struct{}),
	}
	t.wg.Add(1)
	go t.sweeper()
	return t
}

// Close stops the expiry sweeper.
func (t *TUNSrcTable) Close() {
	close(t.done)
	t.wg.Wait()
}

// Register records the original source IP for a flow. Called from the TUN
// read loop with the PRE-SNAT packet fields. Idempotent per key: re-registering
// an existing key is a cheap no-op check so per-packet UDP calls stay light.
func (t *TUNSrcTable) Register(proto byte, dstIP net.IP, dstPort uint16, srcPort uint16, origSrc net.IP) {
	if t == nil || origSrc == nil {
		return
	}
	key := tunSrcKey(proto, dstIP, dstPort, srcPort)
	t.mu.RLock()
	_, exists := t.entries[key]
	t.mu.RUnlock()
	if exists {
		return
	}
	t.mu.Lock()
	if _, exists := t.entries[key]; !exists {
		t.entries[key] = tunSrcEntry{origSrc: origSrc, createdAt: time.Now()}
	}
	t.mu.Unlock()
}

// Lookup returns the original source IP registered for the flow, or nil.
// Called by the netstack forwarder with the POST-SNAT view: the SNAT target
// rewrites src IP to the VIP but preserves the source port, so the key's
// srcPort still matches what the read loop registered.
func (t *TUNSrcTable) Lookup(proto byte, dstIP net.IP, dstPort uint16, srcPort uint16) net.IP {
	if t == nil {
		return nil
	}
	key := tunSrcKey(proto, dstIP, dstPort, srcPort)
	t.mu.RLock()
	e, ok := t.entries[key]
	t.mu.RUnlock()
	if !ok {
		return nil
	}
	return e.origSrc
}

func (t *TUNSrcTable) sweeper() {
	defer t.wg.Done()
	ticker := time.NewTicker(tunSrcSweepEvery)
	defer ticker.Stop()
	for {
		select {
		case <-t.done:
			return
		case now := <-ticker.C:
			cutoff := now.Add(-tunSrcTTL)
			t.mu.Lock()
			for k, e := range t.entries {
				if e.createdAt.Before(cutoff) {
					delete(t.entries, k)
				}
			}
			t.mu.Unlock()
		}
	}
}

func tunSrcKey(proto byte, dstIP net.IP, dstPort uint16, srcPort uint16) string {
	return string(rune(proto)) + ":" + net.JoinHostPort(dstIP.String(), strconv.Itoa(int(dstPort))) + ":" + strconv.Itoa(int(srcPort))
}
