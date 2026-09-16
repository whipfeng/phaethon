package mesh

import (
	"testing"
)

type mockSender struct {
	nodeID string
}

func (s *mockSender) Send(data []byte) error      { return nil }
func (s *mockSender) SendGossip(data []byte)       {}
func (s *mockSender) GetNodeID() string            { return s.nodeID }

func makePeer(id string) PeerSender {
	return &mockSender{nodeID: id}
}

func peerID(p PeerSender) string {
	if p == nil {
		return ""
	}
	return p.GetNodeID()
}

func peersIDs(peers []PeerWithHop) []string {
	var ids []string
	for _, p := range peers {
		ids = append(ids, p.Peer.GetNodeID())
	}
	return ids
}

func TestDomainTrie_InsertAndLookup(t *testing.T) {
	trie := NewDomainTrie()

	trie.Insert("google.com", makePeer("node-a"), nil, 1)
	trie.Insert("api.github.com", makePeer("node-b"), nil, 1)
	trie.Insert("github.com", makePeer("node-c"), nil, 1)

	tests := []struct {
		domain       string
		expectedNode string
		expectedLen  int
	}{
		{"google.com", "node-a", 10},
		{"github.com", "node-c", 10},
		{"api.google.com", "node-a", 10},
		{"www.google.com", "node-a", 10},
		{"api.github.com", "node-b", 14},
		{"www.github.com", "node-c", 10},
		{"example.com", "", 0},
		{"notgoogle.com", "", 0},
		{".google.com", "node-a", 10},
	}

	for _, tt := range tests {
		peers, _, length := trie.Lookup(tt.domain)
		gotNode := ""
		if len(peers) > 0 {
			gotNode = peerID(peers[0].Peer)
		}
		if gotNode != tt.expectedNode {
			t.Errorf("Lookup(%q) nodeID = %q, want %q", tt.domain, gotNode, tt.expectedNode)
		}
		if length != tt.expectedLen {
			t.Errorf("Lookup(%q) length = %d, want %d", tt.domain, length, tt.expectedLen)
		}
	}
}

func TestDomainTrie_LongestMatch(t *testing.T) {
	trie := NewDomainTrie()

	trie.Insert("com", makePeer("root"), nil, 1)
	trie.Insert("google.com", makePeer("google"), nil, 1)
	trie.Insert("api.google.com", makePeer("api"), nil, 1)

	peers, _, _ := trie.Lookup("api.google.com")
	if len(peers) == 0 || peerID(peers[0].Peer) != "api" {
		t.Errorf("expected 'api', got %v", peersIDs(peers))
	}

	peers, _, _ = trie.Lookup("www.google.com")
	if len(peers) == 0 || peerID(peers[0].Peer) != "google" {
		t.Errorf("expected 'google', got %v", peersIDs(peers))
	}

	peers, _, _ = trie.Lookup("example.com")
	if len(peers) == 0 || peerID(peers[0].Peer) != "root" {
		t.Errorf("expected 'root', got %v", peersIDs(peers))
	}
}

func TestDomainTrie_CaseInsensitive(t *testing.T) {
	trie := NewDomainTrie()
	trie.Insert("Google.COM", makePeer("node-a"), nil, 1)

	peers, _, _ := trie.Lookup("API.google.com")
	if len(peers) == 0 || peerID(peers[0].Peer) != "node-a" {
		t.Errorf("case insensitive match failed, got %v", peersIDs(peers))
	}

	peers, _, _ = trie.Lookup("api.GOOGLE.COM")
	if len(peers) == 0 || peerID(peers[0].Peer) != "node-a" {
		t.Errorf("case insensitive match failed, got %v", peersIDs(peers))
	}
}

func TestDomainTrie_LeadingDot(t *testing.T) {
	trie := NewDomainTrie()

	trie.Insert(".google.com", makePeer("node-a"), nil, 1)

	peers, _, _ := trie.Lookup("google.com")
	if len(peers) == 0 || peerID(peers[0].Peer) != "node-a" {
		t.Errorf("leading dot insert failed, got %v", peersIDs(peers))
	}

	peers, _, _ = trie.Lookup("api.google.com")
	if len(peers) == 0 || peerID(peers[0].Peer) != "node-a" {
		t.Errorf("leading dot insert subdomain failed, got %v", peersIDs(peers))
	}
}

func TestDomainTrie_HopPreference(t *testing.T) {
	trie := NewDomainTrie()

	trie.Insert("google.com", makePeer("far"), nil, 5)
	trie.Insert("google.com", makePeer("near"), nil, 1)

	peers, _, _ := trie.Lookup("google.com")
	if len(peers) == 0 || peerID(peers[0].Peer) != "near" {
		t.Errorf("expected lower hop 'near' first, got %v", peersIDs(peers))
	}
	if len(peers) != 2 {
		t.Errorf("expected 2 peers, got %d", len(peers))
	}
}

func TestDomainTrie_LongestMatchOverridesOwn(t *testing.T) {
	trie := NewDomainTrie()

	// Own entry at "phn" (short suffix, no peers = local)
	trie.Insert("phn", nil, nil, 0)
	// Remote entry at "vm.phn" (longer suffix, has peer)
	trie.Insert("vm.phn", makePeer("vm-node"), nil, 1)

	// "vm.phn" should match the longer remote entry, not the shorter own entry
	peers, _, length := trie.Lookup("vm.phn")
	if len(peers) == 0 || peerID(peers[0].Peer) != "vm-node" {
		t.Errorf("Lookup(\"vm.phn\") = %v, want [vm-node] (longest match should win over own)", peersIDs(peers))
	}
	if length != 6 {
		t.Errorf("Lookup(\"vm.phn\") length = %d, want 6", length)
	}

	// "qg.phn" should match the shorter own entry (no "qg.phn" entry exists)
	peers, _, length = trie.Lookup("qg.phn")
	if len(peers) != 0 {
		t.Errorf("Lookup(\"qg.phn\") should be local (empty peers), got %v", peersIDs(peers))
	}
	if length != 3 {
		t.Errorf("Lookup(\"qg.phn\") length = %d, want 3", length)
	}
}

func TestDomainTrie_MultiPeer(t *testing.T) {
	trie := NewDomainTrie()

	trie.Insert("vm.phn", makePeer("peer-a"), nil, 2)
	trie.Insert("vm.phn", makePeer("peer-b"), nil, 1)
	trie.Insert("vm.phn", makePeer("peer-c"), nil, 1)

	peers, _, _ := trie.Lookup("vm.phn")
	if len(peers) != 3 {
		t.Fatalf("expected 3 peers, got %d", len(peers))
	}
	// Should be sorted by hop: peer-b(1), peer-c(1), peer-a(2)
	if peers[0].Hop != 1 || peers[1].Hop != 1 || peers[2].Hop != 2 {
		t.Errorf("peers not sorted by hop: %v", peers)
	}
}
