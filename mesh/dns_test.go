package mesh

import (
	"net"
	"testing"
	"time"
)

// TestDNSCacheCaseInsensitive verifies cache keys are normalized to lowercase:
// resolvers vary in case and a case mismatch must not defeat the cache.
func TestDNSCacheCaseInsensitive(t *testing.T) {
	c := NewDNSCache()
	c.Set("Loc.QG.phn", net.ParseIP("100.0.0.5"), time.Minute)

	if got := c.Get("loc.qg.phn"); got == nil || !got.Equal(net.ParseIP("100.0.0.5")) {
		t.Fatalf("Get(lowercase) = %v, want 100.0.0.5", got)
	}
	if got := c.Get("LOC.QG.PHN"); got == nil || !got.Equal(net.ParseIP("100.0.0.5")) {
		t.Fatalf("Get(uppercase) = %v, want 100.0.0.5", got)
	}

	c.Set("loc.qg.phn", net.ParseIP("100.0.0.6"), time.Minute)
	if got := c.Get("Loc.QG.phn"); got == nil || !got.Equal(net.ParseIP("100.0.0.6")) {
		t.Fatalf("overwrite across cases = %v, want 100.0.0.6", got)
	}
}

// TestDNSCacheExpiry ensures expired entries are not returned.
func TestDNSCacheExpiry(t *testing.T) {
	c := NewDNSCache()
	c.Set("a.phn", net.ParseIP("100.0.0.1"), -time.Second)
	if got := c.Get("a.phn"); got != nil {
		t.Fatalf("expired entry returned %v", got)
	}
}
