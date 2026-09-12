package mesh

import "strings"

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
	nodeID   string // non-empty at terminal nodes
}

// NewDomainTrie creates an empty domain trie.
func NewDomainTrie() *DomainTrie {
	return &DomainTrie{
		root: &trieNode{children: make(map[string]*trieNode)},
	}
}

// Insert adds a domain suffix associated with a nodeID.
// The suffix should be a bare domain like "google.com" (no leading dot).
// It matches the domain itself and all subdomains (e.g., "api.google.com").
func (t *DomainTrie) Insert(suffix string, nodeID string) {
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
	node.nodeID = nodeID
}

// Lookup finds the longest matching suffix for the given domain.
// Returns the nodeID and the matched suffix length (in characters), or ("", 0) if no match.
// The domain should be a bare domain like "api.google.com" (no leading dot).
func (t *DomainTrie) Lookup(domain string) (string, int) {
	domain = strings.TrimPrefix(strings.ToLower(domain), ".")
	labels := splitLabels(domain)

	node := t.root
	bestNodeID := ""
	bestLen := 0
	accumulated := 0

	for i, label := range labels {
		child, ok := node.children[label]
		if !ok {
			break
		}
		node = child
		if i > 0 {
			accumulated += 1 + len(labels[i-1])
		}
		if node.nodeID != "" {
			bestNodeID = node.nodeID
			bestLen = accumulated + len(label)
		}
	}
	return bestNodeID, bestLen
}

// BuildFromTopology constructs a domain trie from all peers' domain suffixes.
func BuildFromTopology(topo *Topology) *DomainTrie {
	trie := NewDomainTrie()
	topo.mu.RLock()
	defer topo.mu.RUnlock()

	for _, peer := range topo.peers {
		for _, entry := range peer.DomainSuffixes {
			trie.Insert(entry.Suffix, entry.SourceNodeID)
		}
	}
	return trie
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
