package tun

import (
	"net"
	"testing"
)

func TestFakeIPPool_DefaultRange(t *testing.T) {
	pool := NewFakeIPPool()

	// Should allocate from 198.18.0.0/15
	ip1 := pool.Lookup("example.com")
	if ip1 == nil {
		t.Fatal("Lookup returned nil")
	}

	// Check it's in the expected range
	if !pool.Contains(ip1) {
		t.Errorf("allocated IP %s not in pool range", ip1)
	}

	// Same domain should return same IP
	ip2 := pool.Lookup("example.com")
	if !ip1.Equal(ip2) {
		t.Errorf("same domain returned different IPs: %s vs %s", ip1, ip2)
	}

	// Different domain should return different IP
	ip3 := pool.Lookup("other.com")
	if ip1.Equal(ip3) {
		t.Errorf("different domains returned same IP: %s", ip1)
	}
}

func TestFakeIPPool_CustomSubnet(t *testing.T) {
	_, subnet, _ := net.ParseCIDR("100.64.0.0/20")
	pool := NewFakeIPPoolWithSubnet(subnet, 3) // skip first 3 (.1=VIP, .2=hostIP, .3=GIP)

	// Should allocate from 100.64.0.4 onwards
	ip1 := pool.Lookup("example.com")
	if ip1 == nil {
		t.Fatal("Lookup returned nil")
	}

	expected := net.ParseIP("100.64.0.4").To4()
	if !ip1.Equal(expected) {
		t.Errorf("first allocation = %s, want %s", ip1, expected)
	}

	// Check Contains
	if !pool.Contains(ip1) {
		t.Errorf("allocated IP %s not in pool range", ip1)
	}

	// Check it's in the subnet
	if !subnet.Contains(ip1) {
		t.Errorf("allocated IP %s not in subnet %s", ip1, subnet)
	}

	// Allocate more IPs
	for i := 0; i < 10; i++ {
		ip := pool.Lookup("test" + string(rune('a'+i)) + ".com")
		if !subnet.Contains(ip) {
			t.Errorf("IP %s not in subnet", ip)
		}
	}
}

func TestFakeIPPool_CustomSubnet_SkipReserved(t *testing.T) {
	_, subnet, _ := net.ParseCIDR("100.64.16.0/20")
	pool := NewFakeIPPoolWithSubnet(subnet, 3)

	// First allocation should be .4 (skip .0=network, .1=VIP, .2=hostIP, .3=GIP)
	ip := pool.Lookup("test.com")
	expected := net.ParseIP("100.64.16.4").To4()
	if !ip.Equal(expected) {
		t.Errorf("first allocation = %s, want %s", ip, expected)
	}
}

func TestFakeIPPool_LookupDomain(t *testing.T) {
	pool := NewFakeIPPool()

	ip := pool.Lookup("example.com")
	domain := pool.LookupDomain(ip.String())
	if domain != "example.com" {
		t.Errorf("LookupDomain = %q, want %q", domain, "example.com")
	}

	// Unknown IP should return empty
	unknown := pool.LookupDomain("1.2.3.4")
	if unknown != "" {
		t.Errorf("unknown IP returned %q, want empty", unknown)
	}
}

func TestFakeIPPool_Release(t *testing.T) {
	pool := NewFakeIPPool()

	ip := pool.Lookup("example.com")
	pool.Release("example.com")

	// IP should be freed
	domain := pool.LookupDomain(ip.String())
	if domain != "" {
		t.Errorf("after release, LookupDomain = %q, want empty", domain)
	}

	// Re-allocating should work
	ip2 := pool.Lookup("other.com")
	if ip2 == nil {
		t.Fatal("Lookup after release returned nil")
	}
}

func TestFakeIPPool_Wraparound(t *testing.T) {
	// Small subnet for testing wraparound
	_, subnet, _ := net.ParseCIDR("10.0.0.0/29") // 8 addresses
	pool := NewFakeIPPoolWithSubnet(subnet, 1)    // skip .0=network, .1

	// Allocate all available IPs
	// /29 = 8 addresses, minus network, broadcast, and 1 skip = 5 usable
	domains := []string{"a.com", "b.com", "c.com", "d.com", "e.com"}
	ips := make(map[string]string)
	for _, d := range domains {
		ip := pool.Lookup(d)
		ips[d] = ip.String()
	}

	// Allocate one more - should wrap around and evict
	ip := pool.Lookup("f.com")
	if ip == nil {
		t.Fatal("wraparound allocation returned nil")
	}

	// The pool should still be functional
	if !subnet.Contains(ip) {
		t.Errorf("wraparound IP %s not in subnet", ip)
	}
}

func TestFakeIPPool_Contains(t *testing.T) {
	_, subnet, _ := net.ParseCIDR("100.64.0.0/20")
	pool := NewFakeIPPoolWithSubnet(subnet, 3)

	tests := []struct {
		ip       string
		expected bool
	}{
		{"100.64.0.4", true},
		{"100.64.0.100", true},
		{"100.64.15.255", true},
		{"100.64.0.0", true},  // network address is in range
		{"100.64.16.0", false}, // outside subnet
		{"100.63.255.255", false},
		{"8.8.8.8", false},
	}

	for _, tt := range tests {
		ip := net.ParseIP(tt.ip)
		if got := pool.Contains(ip); got != tt.expected {
			t.Errorf("Contains(%s) = %v, want %v", tt.ip, got, tt.expected)
		}
	}
}
