package mesh

import (
	"strings"
)

// NodeTrie is a simplified trie that only stores nodeID for each suffix.
// Used for both static and dynamic domain routing.
type NodeTrie struct {
	root *nodeTrieNode
}

type nodeTrieNode struct {
	children map[string]*nodeTrieNode
	nodeID   string // empty if no entry at this node
}

func NewNodeTrie() *NodeTrie {
	return &NodeTrie{
		root: &nodeTrieNode{children: make(map[string]*nodeTrieNode)},
	}
}

// Insert adds a nodeID for a domain suffix.
func (t *NodeTrie) Insert(suffix string, nodeID string) {
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
	node.nodeID = nodeID
}

// Lookup finds the longest matching suffix for the given domain.
// Returns the nodeID and the matched suffix length.
// Returns ("", 0) if no match.
func (t *NodeTrie) Lookup(domain string) (string, int) {
	domain = strings.TrimPrefix(strings.ToLower(domain), ".")
	labels := splitLabels(domain)

	node := t.root
	var bestNodeID string
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
		if node.nodeID != "" {
			bestNodeID = node.nodeID
			bestLen = accumulated
		}
	}

	return bestNodeID, bestLen
}
