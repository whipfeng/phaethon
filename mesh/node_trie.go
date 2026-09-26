package mesh

import (
	"strings"
)

// NodeTrie is a simplified trie that stores route entries for each suffix.
// Used for both static and dynamic domain routing.
type NodeTrie struct {
	root *nodeTrieNode
}

type nodeTrieNode struct {
	children map[string]*nodeTrieNode
	entries  []RouteEntry // route entries (nodeID + source) at this node
}

func NewNodeTrie() *NodeTrie {
	return &NodeTrie{
		root: &nodeTrieNode{children: make(map[string]*nodeTrieNode)},
	}
}

// Insert adds a route entry for a domain suffix.
func (t *NodeTrie) Insert(suffix string, nodeID string, source RouteSource) {
	suffix = strings.TrimPrefix(strings.ToLower(suffix), ".")
	labels := splitLabels(suffix)

	node := t.root
	for _, label := range labels {
		child, ok := node.children[label]
		if !ok {
			child = &nodeTrieNode{children: make(map[string]*nodeTrieNode)}
			node.children[label] = child
		}
		node = child
	}
	// Add entry if not already present
	for _, e := range node.entries {
		if e.NodeID == nodeID {
			return // already exists
		}
	}
	node.entries = append(node.entries, RouteEntry{NodeID: nodeID, Source: source})
}

// Lookup finds the longest matching suffix for the given domain.
// Returns the route entries and the matched suffix length.
// Returns (nil, 0) if no match.
func (t *NodeTrie) Lookup(domain string) ([]RouteEntry, int) {
	domain = strings.TrimPrefix(strings.ToLower(domain), ".")
	labels := splitLabels(domain)

	node := t.root
	var bestEntries []RouteEntry
	bestLen := 0
	accumulated := 0

	for i, label := range labels {
		child, ok := node.children[label]
		if !ok {
			break
		}
		node = child
		accumulated += len(label)
		if i > 0 {
			accumulated++ // for the dot
		}
		if len(node.entries) > 0 {
			bestEntries = node.entries
			bestLen = accumulated
		}
	}

	return bestEntries, bestLen
}
