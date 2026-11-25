package mesh

import (
	"container/list"
	"sync"
	"time"
)

// stickyLRU bounds the target→egress-node cache. Targets are arbitrary IPs and
// domains; without a bound, scans or large domain sets grow the map forever.
const stickyLRUMax = 65536

// stickyRebalanceGrace is how long an idle selection may survive a topology
// generation change before it is re-hashed onto the current owner set. Active
// targets keep their owner; only idle ones migrate, so newly added owners pick
// up traffic without disturbing live sessions.
const stickyRebalanceGrace = 10 * time.Minute

type stickyLRUEntry struct {
	key      selectionKey
	value    string
	gen      uint64
	lastUsed time.Time
}

type stickyLRU struct {
	mu    sync.Mutex
	max   int
	order *list.List
	items map[selectionKey]*list.Element
}

func newStickyLRU(max int) *stickyLRU {
	if max <= 0 {
		max = stickyLRUMax
	}
	return &stickyLRU{
		max:   max,
		order: list.New(),
		items: make(map[selectionKey]*list.Element, max),
	}
}

// Get returns the cached value and refreshes its recency. An entry created
// under an older topology generation is a miss once it has been idle for grace,
// so it gets re-selected against the current owner set.
func (c *stickyLRU) Get(key selectionKey, gen uint64, grace time.Duration) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return "", false
	}
	e := el.Value.(*stickyLRUEntry)
	now := time.Now()
	if e.gen != gen && now.Sub(e.lastUsed) >= grace {
		c.order.Remove(el)
		delete(c.items, key)
		return "", false
	}
	e.lastUsed = now
	c.order.MoveToFront(el)
	return e.value, true
}

// Put inserts or updates a value, evicting the least-recently-used entry when
// the cache exceeds its bound.
func (c *stickyLRU) Put(key selectionKey, value string, gen uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		e := el.Value.(*stickyLRUEntry)
		e.value = value
		e.gen = gen
		e.lastUsed = time.Now()
		c.order.MoveToFront(el)
		return
	}
	el := c.order.PushFront(&stickyLRUEntry{key: key, value: value, gen: gen, lastUsed: time.Now()})
	c.items[key] = el
	for len(c.items) > c.max {
		back := c.order.Back()
		if back == nil {
			break
		}
		c.order.Remove(back)
		delete(c.items, back.Value.(*stickyLRUEntry).key)
	}
}
