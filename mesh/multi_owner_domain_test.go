package mesh

import (
	"fmt"
	"net"
	"sort"
	"testing"
)

func newTestMeshManagerWithDomains(t *testing.T, nodeID, subnetStr string, domains []string) *MeshManager {
	t.Helper()
	_, subnet, err := net.ParseCIDR(subnetStr)
	if err != nil {
		t.Fatalf("parse subnet %q: %v", subnetStr, err)
	}
	_, meshNet, err := net.ParseCIDR("100.64.0.0/10")
	if err != nil {
		t.Fatal(err)
	}
	return NewMeshManager(nodeID, net.ParseIP("100.64.1.1"), nil, subnet, subnetStr, domains, nil, meshNet, 24)
}

func domainTreeOwners(nodes []map[string]interface{}, full string) []string {
	for _, n := range nodes {
		if n["full"] == full {
			raw, _ := n["owners"].([]string)
			return raw
		}
		children, _ := n["children"].([]map[string]interface{})
		if got := domainTreeOwners(children, full); got != nil {
			return got
		}
	}
	return nil
}

// TestMultipleOwnersSameDomainSuffix verifies that two nodes declaring the
// same domain suffix both survive gossip aggregation, the admin tree and the
// relayed gossip summary — matching the multi-owner route behavior.
func TestMultipleOwnersSameDomainSuffix(t *testing.T) {
	m := newTestMeshManagerWithDomains(t, "self", "100.64.1.0/24", nil)

	ms9 := newMockPeerSender("ms9")
	ms10 := newMockPeerSender("ms10")
	m.topology.RegisterPeer(ms9)
	m.topology.RegisterPeer(ms10)
	m.topology.UpdateGossip(ms9, GossipInfo{
		DomainSuffixes: []GossipDomainSuffix{{Suffix: "git.migu.cn", NodeID: "ms9"}},
		ClaimedSubnets: []GossipClaimedSubnet{
			{Subnet: "100.64.9.0/24", NodeID: "ms9", Hop: 0},
		},
	})
	m.topology.UpdateGossip(ms10, GossipInfo{
		DomainSuffixes: []GossipDomainSuffix{{Suffix: "git.migu.cn", NodeID: "ms10"}},
		ClaimedSubnets: []GossipClaimedSubnet{
			{Subnet: "100.64.10.0/24", NodeID: "ms10", Hop: 0},
		},
	})

	m.recomputeRoutes()

	owners := domainTreeOwners(m.GetDomainRoutes(), "git.migu.cn")
	sort.Strings(owners)
	if len(owners) != 2 || owners[0] != "ms10" || owners[1] != "ms9" {
		t.Fatalf("admin tree owners = %v, want [ms10 ms9]", owners)
	}

	info := m.BuildGossipInfo()
	var relayed []string
	for _, ds := range info.DomainSuffixes {
		if ds.Suffix == "git.migu.cn" {
			relayed = append(relayed, ds.NodeID)
		}
	}
	sort.Strings(relayed)
	if len(relayed) != 2 || relayed[0] != "ms10" || relayed[1] != "ms9" {
		t.Fatalf("relayed domain suffixes = %v, want [ms10 ms9]", relayed)
	}
}

// TestLocalDomainSuffixResolvesLocally verifies that a node declaring a
// suffix still resolves it with its own Fake-IP pool even when peers declare
// the same suffix, while the admin tree lists every owner.
func TestLocalDomainSuffixResolvesLocally(t *testing.T) {
	m := newTestMeshManagerWithDomains(t, "self", "100.64.1.0/24", []string{"git.migu.cn"})

	ms9 := newMockPeerSender("ms9")
	m.topology.RegisterPeer(ms9)
	m.topology.UpdateGossip(ms9, GossipInfo{
		DomainSuffixes: []GossipDomainSuffix{{Suffix: "git.migu.cn", NodeID: "ms9"}},
		ClaimedSubnets: []GossipClaimedSubnet{
			{Subnet: "100.64.9.0/24", NodeID: "ms9", Hop: 0},
		},
	})

	m.recomputeRoutes()

	owners := domainTreeOwners(m.GetDomainRoutes(), "git.migu.cn")
	sort.Strings(owners)
	if len(owners) != 2 || owners[0] != "ms9" || owners[1] != "self" {
		t.Fatalf("admin tree owners = %v, want [ms9 self]", owners)
	}

	subnet, needsFail := m.ResolveDomainSubnet("git.migu.cn")
	if subnet != nil || needsFail {
		t.Fatalf("local suffix resolved to remote subnet %v (needsFail=%v), want local pool", subnet, needsFail)
	}
}

// TestDomainSelectionStaticExclusive verifies domain routing mirrors prefix
// routing: a static owner wins outright, dynamic owners only participate when
// no static entry exists.
func TestDomainSelectionStaticExclusive(t *testing.T) {
	m := newTestMeshManagerWithDomains(t, "self", "100.64.1.0/24", nil)
	entries := []RouteEntry{
		{NodeID: "ms10", Source: RouteSourceDynamic},
		{NodeID: "ms9", Source: RouteSourceStatic},
	}

	for i := 0; i < 64; i++ {
		domain := fmt.Sprintf("host%d.example.com", i)
		if got := m.selectEgressNodeIDForDomain(domain, entries); got != "ms9" {
			t.Fatalf("domain %s selected dynamic owner %q despite static entry", domain, got)
		}
	}
}
