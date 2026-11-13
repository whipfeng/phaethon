package mesh

import (
	"container/list"
	"sync"
)

// stickyLRU bounds the target→egress-node cache. Targets are arbitrary IPs and
// domains; without a bound, scans or large domain sets grow the map forever.
const stickyLRUMax = 65536

type stickyLRUEntry struct {
	key   string
	value string
}

type stickyLRU struct {
	mu    sync.Mutex
	max   int
	order *list.List
	items map[string]*list.Element
}

func newStickyLRU(max int) *stickyLRU {
	if max <= 0 {
		max = stickyLRUMax
	}
	return &stickyLRU{
		max:   max,
		order: list.New(),
		items: make(map[string]*list.Element, max),
	}
}

// Get returns the cached value and refreshes its recency.
func (c *stickyLRU) Get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return "", false
	}
	c.order.MoveToFront(el)
	return el.Value.(*stickyLRUEntry).value, true
}

// Put inserts or updates a value, evicting the least-recently-used entry when
// the cache exceeds its bound.
func (c *stickyLRU) Put(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		el.Value.(*stickyLRUEntry).value = value
		c.order.MoveToFront(el)
		return
	}
	el := c.order.PushFront(&stickyLRUEntry{key: key, value: value})
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
