package mesh

import (
	"testing"
)

func TestDomainTrie_InsertAndLookup(t *testing.T) {
	trie := NewDomainTrie()

	// Insert some suffixes
	trie.Insert("google.com", "node-a")
	trie.Insert("api.github.com", "node-b")
	trie.Insert("github.com", "node-c")

	tests := []struct {
		domain       string
		expectedNode string
		expectedLen  int
	}{
		// Exact match
		{"google.com", "node-a", 10},
		{"github.com", "node-c", 10},
		// Subdomain match
		{"api.google.com", "node-a", 10},
		{"www.google.com", "node-a", 10},
		{"api.github.com", "node-b", 14}, // longest match: api.github.com > github.com
		{"www.github.com", "node-c", 10}, // matches github.com, not api.github.com
		// No match
		{"example.com", "", 0},
		{"notgoogle.com", "", 0}, // should NOT match google.com
		// With leading dot (should be normalized)
		{".google.com", "node-a", 10},
	}

	for _, tt := range tests {
		nodeID, length := trie.Lookup(tt.domain)
		if nodeID != tt.expectedNode {
			t.Errorf("Lookup(%q) nodeID = %q, want %q", tt.domain, nodeID, tt.expectedNode)
		}
		if length != tt.expectedLen {
			t.Errorf("Lookup(%q) length = %d, want %d", tt.domain, length, tt.expectedLen)
		}
	}
}

func TestDomainTrie_LongestMatch(t *testing.T) {
	trie := NewDomainTrie()

	// Insert overlapping suffixes
	trie.Insert("com", "root")
	trie.Insert("google.com", "google")
	trie.Insert("api.google.com", "api")

	// Should match longest
	nodeID, _ := trie.Lookup("api.google.com")
	if nodeID != "api" {
		t.Errorf("expected 'api', got %q", nodeID)
	}

	nodeID, _ = trie.Lookup("www.google.com")
	if nodeID != "google" {
		t.Errorf("expected 'google', got %q", nodeID)
	}

	nodeID, _ = trie.Lookup("example.com")
	if nodeID != "root" {
		t.Errorf("expected 'root', got %q", nodeID)
	}
}

func TestDomainTrie_CaseInsensitive(t *testing.T) {
	trie := NewDomainTrie()
	trie.Insert("Google.COM", "node-a")

	nodeID, _ := trie.Lookup("API.google.com")
	if nodeID != "node-a" {
		t.Errorf("case insensitive match failed, got %q", nodeID)
	}

	nodeID, _ = trie.Lookup("api.GOOGLE.COM")
	if nodeID != "node-a" {
		t.Errorf("case insensitive match failed, got %q", nodeID)
	}
}

func TestDomainTrie_LeadingDot(t *testing.T) {
	trie := NewDomainTrie()

	// Insert with leading dot (should be stripped)
	trie.Insert(".google.com", "node-a")

	// Should match without dot
	nodeID, _ := trie.Lookup("google.com")
	if nodeID != "node-a" {
		t.Errorf("leading dot insert failed, got %q", nodeID)
	}

	nodeID, _ = trie.Lookup("api.google.com")
	if nodeID != "node-a" {
		t.Errorf("leading dot insert subdomain failed, got %q", nodeID)
	}
}
