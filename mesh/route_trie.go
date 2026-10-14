package mesh

import (
	"net"
	"sync"
)

// routeTrieNode represents a node in the IP prefix trie.
type routeTrieNode struct {
	children [2]*routeTrieNode // 0 and 1 branches
	route    *MeshRoute        // non-nil if this node has a route
	prefixLen int              // prefix length for this node
}

// routeTrie is an IP prefix trie for longest-prefix-match routing.
type routeTrie struct {
	mu   sync.RWMutex
	root *routeTrieNode
}

// newRouteTrie creates an empty route trie.
func newRouteTrie() *routeTrie {
	return &routeTrie{
		root: &routeTrieNode{},
	}
}

// Insert adds a route to the trie.
func (t *routeTrie) Insert(prefix *net.IPNet, route *MeshRoute) {
	t.mu.Lock()
	defer t.mu.Unlock()

	ip := prefix.IP.To4()
	if ip == nil {
		return // Only support IPv4 for now
	}

	prefixLen, _ := prefix.Mask.Size()
	node := t.root

	// Traverse/create path based on prefix bits
	for i := 0; i < prefixLen; i++ {
		bit := (ip[i/8] >> (7 - uint(i%8))) & 1
		if node.children[bit] == nil {
			node.children[bit] = &routeTrieNode{}
		}
		node = node.children[bit]
	}

	// Store route at this node
	node.route = route
	node.prefixLen = prefixLen
}

// Lookup finds the longest-prefix-match route for the given IP.
func (t *routeTrie) Lookup(ip net.IP) *MeshRoute {
	t.mu.RLock()
	defer t.mu.RUnlock()

	ip4 := ip.To4()
	if ip4 == nil {
		return nil
	}

	var bestMatch *MeshRoute
	node := t.root

	// Check root (default route)
	if node.route != nil {
		bestMatch = node.route
	}

	// Traverse based on IP bits
	for i := 0; i < 32; i++ {
		bit := (ip4[i/8] >> (7 - uint(i%8))) & 1
		if node.children[bit] == nil {
			break
		}
		node = node.children[bit]

		// Update best match if this node has a route
		if node.route != nil {
			bestMatch = node.route
		}
	}

	return bestMatch
}

// Remove removes a route from the trie (not implemented for now).
// Routes are rebuilt on each recomputeRoutes, so removal is not needed.
func (t *routeTrie) Remove(prefix *net.IPNet) {
	// Not implemented - trie is rebuilt on each route computation
}

// Clear removes all routes from the trie.
func (t *routeTrie) Clear() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.root = &routeTrieNode{}
}
