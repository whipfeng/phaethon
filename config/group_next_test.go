package config

import (
	"testing"
)

func TestProxyGroupNextSubscriptionCandidates(t *testing.T) {
	man := &Proxy{Name: "man", Type: ProxySOCKS5, Server: "127.0.0.1", Port: 1080}
	sub := &Subscription{Name: "sub"}
	sub.SetNodes([]*Proxy{
		{Name: "sub1", Type: ProxyTROJAN, Server: "1.1.1.1", Port: 443},
		{Name: "sub2", Type: ProxyTROJAN, Server: "2.2.2.2", Port: 443},
	})

	g := &ProxyGroup{
		Name:          "g",
		Type:          GroupSelect,
		ManualProxies: []string{"man"},
		Subscription:  "sub",
		ResolveProxy: func(name string) *Proxy {
			if name == "man" {
				return man
			}
			return nil
		},
		ResolveSubscription: func(name string) *Subscription {
			if name == "sub" {
				return sub
			}
			return nil
		},
	}
	if err := g.Init(); err != nil {
		t.Fatal(err)
	}
	p := g.Next()
	if p == nil {
		t.Fatal("expected proxy, got nil")
	}
	if p.Name != "sub1" {
		t.Fatalf("expected sub1, got %s", p.Name)
	}

	if err := g.SetActiveMember("sub2"); err != nil {
		t.Fatal(err)
	}
	p = g.Next()
	if p == nil || p.Name != "sub2" {
		t.Fatalf("expected sub2 after active switch, got %v", p)
	}

	for i := 0; i < 3; i++ {
		g.SetHealth("sub:sub1", false, 0)
		g.SetHealth("sub:sub2", false, 0)
	}
	p = g.Next()
	if p == nil || p.Name != "sub2" {
		t.Fatalf("expected active sub2 even when dead, got %v", p)
	}
}

func TestProxyGroupNextManualWhenNoSubscription(t *testing.T) {
	man := &Proxy{Name: "man", Type: ProxySOCKS5, Server: "127.0.0.1", Port: 1080}
	g := &ProxyGroup{
		Name:          "g",
		Type:          GroupSelect,
		ManualProxies: []string{"man"},
		ResolveProxy: func(name string) *Proxy {
			if name == "man" {
				return man
			}
			return nil
		},
	}
	if err := g.Init(); err != nil {
		t.Fatal(err)
	}
	p := g.Next()
	if p == nil || p.Name != "man" {
		t.Fatalf("expected manual man, got %v", p)
	}
}

func TestProxyGroupNextLoadBalanceUsesAllMembers(t *testing.T) {
	man := &Proxy{Name: "man", Type: ProxySOCKS5, Server: "127.0.0.1", Port: 1080}
	sub := &Subscription{Name: "sub"}
	sub.SetNodes([]*Proxy{
		{Name: "sub1", Type: ProxyTROJAN, Server: "1.1.1.1", Port: 443},
		{Name: "sub2", Type: ProxyTROJAN, Server: "2.2.2.2", Port: 443},
	})

	g := &ProxyGroup{
		Name:          "g",
		Type:          GroupLoadBalance,
		ManualProxies: []string{"man"},
		Subscription:  "sub",
		ResolveProxy: func(name string) *Proxy {
			if name == "man" {
				return man
			}
			return nil
		},
		ResolveSubscription: func(name string) *Subscription {
			if name == "sub" {
				return sub
			}
			return nil
		},
	}
	if err := g.Init(); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for i := 0; i < 10; i++ {
		p := g.Next()
		if p == nil {
			t.Fatalf("load-balance should return a member, got nil")
		}
		if p.Name != "sub1" && p.Name != "sub2" && p.Name != "man" {
			t.Fatalf("unexpected load-balance member: %v", p)
		}
		seen[p.Name] = true
	}
	if !seen["sub1"] || !seen["sub2"] || !seen["man"] {
		t.Fatalf("load-balance should cycle through all members, got %v", seen)
	}
}

func TestProxyGroupNextNestedGroupSkipsNil(t *testing.T) {
	p1 := &Proxy{Name: "p1", Type: ProxySOCKS5, Server: "127.0.0.1", Port: 1080}
	empty := &ProxyGroup{
		Name:          "empty",
		Type:          GroupSelect,
		ManualProxies: []string{},
	}
	outer := &ProxyGroup{
		Name:          "outer",
		Type:          GroupSelect,
		ManualProxies: []string{"empty", "p1"},
		ResolveProxy: func(name string) *Proxy {
			if name == "p1" {
				return p1
			}
			return nil
		},
		ResolveGroup: func(name string) *ProxyGroup {
			if name == "empty" {
				return empty
			}
			return nil
		},
	}
	if err := outer.Init(); err != nil {
		t.Fatal(err)
	}
	if err := empty.Init(); err != nil {
		t.Fatal(err)
	}
	empty.ResolveGroup = outer.ResolveGroup
	empty.ResolveProxy = outer.ResolveProxy

	p := outer.Next()
	if p == nil || p.Name != "p1" {
		t.Fatalf("expected p1 after skipping empty nested group, got %v", p)
	}
}

func TestProxyGroupNextNestedGroupCycle(t *testing.T) {
	a := &ProxyGroup{Name: "a", Type: GroupSelect, ManualProxies: []string{"b"}}
	b := &ProxyGroup{Name: "b", Type: GroupSelect, ManualProxies: []string{"a"}}
	a.ResolveGroup = func(name string) *ProxyGroup {
		if name == "b" {
			return b
		}
		return nil
	}
	b.ResolveGroup = func(name string) *ProxyGroup {
		if name == "a" {
			return a
		}
		return nil
	}
	if err := a.Init(); err != nil {
		t.Fatal(err)
	}
	if err := b.Init(); err != nil {
		t.Fatal(err)
	}

	p := a.Next()
	if p != nil {
		t.Fatalf("expected nil for cyclic group reference, got %v", p)
	}
}

func TestProxyGroupSubscriptionFilter(t *testing.T) {
	sub := &Subscription{Name: "sub"}
	sub.SetNodes([]*Proxy{
		{Name: "hk-node", Type: ProxyTROJAN, Server: "1.1.1.1", Port: 443},
		{Name: "us-node", Type: ProxyTROJAN, Server: "2.2.2.2", Port: 443},
		{Name: "hk-fast", Type: ProxyTROJAN, Server: "3.3.3.3", Port: 443},
	})

	g := &ProxyGroup{
		Name:               "g",
		Type:               GroupSelect,
		Subscription:       "sub",
		SubscriptionFilter: "hk",
		ResolveSubscription: func(name string) *Subscription {
			if name == "sub" {
				return sub
			}
			return nil
		},
	}
	if err := g.Init(); err != nil {
		t.Fatal(err)
	}
	candidates := g.SubscriptionCandidates()
	if len(candidates) != 2 {
		t.Fatalf("expected 2 hk candidates, got %v", candidates)
	}
	p := g.Next()
	if p == nil || (p.Name != "hk-node" && p.Name != "hk-fast") {
		t.Fatalf("expected hk candidate, got %v", p)
	}
}

func TestProxyGroupActiveMemberFallbackWhenMissing(t *testing.T) {
	g := &ProxyGroup{
		Name:          "g",
		Type:          GroupSelect,
		ManualProxies: []string{"missing", "p1"},
		ActiveMember:  "missing",
		ResolveProxy: func(name string) *Proxy {
			if name == "p1" {
				return &Proxy{Name: "p1", Type: ProxySOCKS5, Server: "127.0.0.1", Port: 1080}
			}
			return nil
		},
	}
	if err := g.Init(); err != nil {
		t.Fatal(err)
	}
	p := g.Next()
	if p == nil || p.Name != "p1" {
		t.Fatalf("expected fallback to first valid member p1, got %v", p)
	}
}
