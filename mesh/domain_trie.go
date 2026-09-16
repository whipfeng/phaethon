package mesh

import (
	"net"
	"sort"
	"strings"
)

type DomainTrie struct {
	root *trieNode
}

type trieNode struct {
	children map[string]*trieNode
	hasEntry bool
	peers    []PeerWithHop
	subnet   *net.IPNet
}

func NewDomainTrie() *DomainTrie {
	return &DomainTrie{
		root: &trieNode{children: make(map[string]*trieNode)},
	}
}

// Insert adds a peer for a domain suffix. Pass sender=nil for local entries.
// Multiple peers for the same suffix are accumulated and sorted by hop.
func (t *DomainTrie) Insert(suffix string, sender PeerSender, subnet *net.IPNet, hop int) {
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
	node.hasEntry = true
	node.subnet = subnet

	if sender == nil {
		node.peers = nil
		return
	}

	nodeID := sender.GetNodeID()
	for i, p := range node.peers {
		if p.Peer.GetNodeID() == nodeID {
			if hop < p.Hop {
				node.peers[i].Hop = hop
			}
			sort.Slice(node.peers, func(i, j int) bool {
				return node.peers[i].Hop < node.peers[j].Hop
			})
			return
		}
	}
	node.peers = append(node.peers, PeerWithHop{Peer: sender, Hop: hop})
	sort.Slice(node.peers, func(i, j int) bool {
		return node.peers[i].Hop < node.peers[j].Hop
	})
}

// Lookup finds the longest matching suffix for the given domain.
// Returns the peers array, the owning node's subnet, and the matched suffix length.
// len(peers)==0 means local entry. Returns (nil, nil, 0) if no match.
func (t *DomainTrie) Lookup(domain string) ([]PeerWithHop, *net.IPNet, int) {
	domain = strings.TrimPrefix(strings.ToLower(domain), ".")
	labels := splitLabels(domain)

	node := t.root
	var bestPeers []PeerWithHop
	var bestSubnet *net.IPNet
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
		if node.hasEntry {
			suffixLen := accumulated + len(label)
			if suffixLen > bestLen {
				bestLen = suffixLen
				bestPeers = node.peers
				bestSubnet = node.subnet
			}
		}
	}
	if bestLen == 0 {
		return nil, nil, 0
	}
	return bestPeers, bestSubnet, bestLen
}

func splitLabels(domain string) []string {
	parts := strings.Split(domain, ".")
	n := len(parts)
	reversed := make([]string, n)
	for i := 0; i < n; i++ {
		reversed[i] = parts[n-1-i]
	}
	return reversed
}
