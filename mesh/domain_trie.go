package mesh

import (
	"net"
	"strings"
)

// DomainTrie supports longest-suffix matching for domain names.
// It uses label-based matching (split by '.'), similar to DNS resolution.
// For example, "google.com" is stored as root → "com" → "google",
// and "api.google.com" extends to root → "com" → "google" → "api".
// This naturally prevents partial label matches like "notgoogle.com"
// matching "google.com".
type DomainTrie struct {
	root *trieNode
}

type trieNode struct {
	children map[string]*trieNode
	hasEntry bool      // true if this node has a domain entry
	nextHop  *PeerInfo // next-hop peer (nil = own entry)
	subnet   *net.IPNet // Fake-IP subnet of the owning node (nil = own entry)
	hop      int       // hop count
}

// NewDomainTrie creates an empty domain trie.
func NewDomainTrie() *DomainTrie {
	return &DomainTrie{
		root: &trieNode{children: make(map[string]*trieNode)},
	}
}

// Insert adds a domain suffix associated with a next-hop peer, subnet, and hop count.
// If the suffix already exists, keeps the entry with the lower hop count.
// Pass nextHop=nil for own entries (hop should be 0).
func (t *DomainTrie) Insert(suffix string, nextHop *PeerInfo, subnet *net.IPNet, hop int) {
	suffix = strings.TrimPrefix(strings.ToLower(suffix), ".")
	labels := splitLabels(suffix)

	node := t.root
	for _, label := range labels {
		child, ok := node.children[label]
		if !ok {
			child = &trieNode{children: make(map[string]*trieNode)}
			node.children[label] = child
		}
		node = child
	}
	if !node.hasEntry || hop < node.hop {
		node.hasEntry = true
		node.nextHop = nextHop
		node.subnet = subnet
		node.hop = hop
	}
}

// Lookup finds the longest matching suffix for the given domain.
// Returns the next-hop peer, the owning node's subnet, and the matched suffix length (in characters).
// Returns (nil, nil, suffixLen) if the match is an own entry (this node is the gateway).
// Returns (nil, nil, 0) if no match at all.
func (t *DomainTrie) Lookup(domain string) (*PeerInfo, *net.IPNet, int) {
	domain = strings.TrimPrefix(strings.ToLower(domain), ".")
	labels := splitLabels(domain)

	node := t.root
	var bestNextHop *PeerInfo
	var bestSubnet *net.IPNet
	bestLen := 0
	accumulated := 0
	foundOwn := false

	for i, label := range labels {
		child, ok := node.children[label]
		if !ok {
			break
		}
		node = child
		if i > 0 {
			accumulated += 1 + len(labels[i-1])
		}
		if node.hasEntry {
			suffixLen := accumulated + len(label)
			if node.nextHop == nil {
				// Own entry — always prefer (lowest possible hop = 0)
				foundOwn = true
				bestLen = suffixLen
			} else if !foundOwn {
				bestNextHop = node.nextHop
				bestSubnet = node.subnet
				bestLen = suffixLen
			}
		}
	}
	if !foundOwn && bestNextHop == nil {
		return nil, nil, 0
	}
	return bestNextHop, bestSubnet, bestLen
}

// splitLabels splits a domain into labels in reverse order (TLD first).
// "api.google.com" → ["com", "google", "api"]
func splitLabels(domain string) []string {
	parts := strings.Split(domain, ".")
	n := len(parts)
	reversed := make([]string, n)
	for i := 0; i < n; i++ {
		reversed[i] = parts[n-1-i]
	}
	return reversed
}
