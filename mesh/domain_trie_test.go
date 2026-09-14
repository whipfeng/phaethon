package mesh

import (
	"testing"
)

type mockSender struct {
	nodeID string
}

func (s *mockSender) Send(data []byte) error { return nil }
func (s *mockSender) GetNodeID() string      { return s.nodeID }

func makePeer(id string) *PeerInfo {
	return &PeerInfo{Sender: &mockSender{nodeID: id}}
}

func peerID(p *PeerInfo) string {
	if p == nil {
		return ""
	}
	return p.NodeID()
}

func TestDomainTrie_InsertAndLookup(t *testing.T) {
	trie := NewDomainTrie()

	trie.Insert("google.com", makePeer("node-a"), 1)
	trie.Insert("api.github.com", makePeer("node-b"), 1)
	trie.Insert("github.com", makePeer("node-c"), 1)

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
		nextHop, length := trie.Lookup(tt.domain)
		if peerID(nextHop) != tt.expectedNode {
			t.Errorf("Lookup(%q) nodeID = %q, want %q", tt.domain, peerID(nextHop), tt.expectedNode)
		}
		if length != tt.expectedLen {
			t.Errorf("Lookup(%q) length = %d, want %d", tt.domain, length, tt.expectedLen)
		}
	}
}

func TestDomainTrie_LongestMatch(t *testing.T) {
	trie := NewDomainTrie()

	trie.Insert("com", makePeer("root"), 1)
	trie.Insert("google.com", makePeer("google"), 1)
	trie.Insert("api.google.com", makePeer("api"), 1)

	nextHop, _ := trie.Lookup("api.google.com")
	if peerID(nextHop) != "api" {
		t.Errorf("expected 'api', got %q", peerID(nextHop))
	}

	nextHop, _ = trie.Lookup("www.google.com")
	if peerID(nextHop) != "google" {
		t.Errorf("expected 'google', got %q", peerID(nextHop))
	}

	nextHop, _ = trie.Lookup("example.com")
	if peerID(nextHop) != "root" {
		t.Errorf("expected 'root', got %q", peerID(nextHop))
	}
}

func TestDomainTrie_CaseInsensitive(t *testing.T) {
	trie := NewDomainTrie()
	trie.Insert("Google.COM", makePeer("node-a"), 1)

	nextHop, _ := trie.Lookup("API.google.com")
	if peerID(nextHop) != "node-a" {
		t.Errorf("case insensitive match failed, got %q", peerID(nextHop))
	}

	nextHop, _ = trie.Lookup("api.GOOGLE.COM")
	if peerID(nextHop) != "node-a" {
		t.Errorf("case insensitive match failed, got %q", peerID(nextHop))
	}
}

func TestDomainTrie_LeadingDot(t *testing.T) {
	trie := NewDomainTrie()

	trie.Insert(".google.com", makePeer("node-a"), 1)

	nextHop, _ := trie.Lookup("google.com")
	if peerID(nextHop) != "node-a" {
		t.Errorf("leading dot insert failed, got %q", peerID(nextHop))
	}

	nextHop, _ = trie.Lookup("api.google.com")
	if peerID(nextHop) != "node-a" {
		t.Errorf("leading dot insert subdomain failed, got %q", peerID(nextHop))
	}
}

func TestDomainTrie_HopPreference(t *testing.T) {
	trie := NewDomainTrie()

	trie.Insert("google.com", makePeer("far"), 5)
	trie.Insert("google.com", makePeer("near"), 1)

	nextHop, _ := trie.Lookup("google.com")
	if peerID(nextHop) != "near" {
		t.Errorf("expected lower hop 'near', got %q", peerID(nextHop))
	}
}
