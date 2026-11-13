package mesh

import (
	"net"
	"sort"
	"testing"
)

// newTestMeshManager builds a minimal manager for route table tests.
func newTestMeshManager(t *testing.T, nodeID, subnetStr string, advertise []string) *MeshManager {
	t.Helper()
	_, subnet, err := net.ParseCIDR(subnetStr)
	if err != nil {
		t.Fatalf("parse subnet %q: %v", subnetStr, err)
	}
	_, meshNet, err := net.ParseCIDR("100.64.0.0/10")
	if err != nil {
		t.Fatal(err)
	}
	return NewMeshManager(nodeID, net.ParseIP("100.64.1.1"), nil, subnet, subnetStr, nil, advertise, meshNet, 24)
}

// TestSelectEgressNodeIDDeterministic verifies that hash selection does not
// depend on the order in which gossip copies of the same route arrive.
func TestSelectEgressNodeIDDeterministic(t *testing.T) {
	target := net.ParseIP("10.161.88.9")
	entries := []RouteEntry{
		{NodeID: "ms9", Source: RouteSourceDynamic},
		{NodeID: "ms10", Source: RouteSourceDynamic},
	}

	m1 := newTestMeshManager(t, "self", "100.64.1.0/24", nil)
	m2 := newTestMeshManager(t, "self", "100.64.1.0/24", nil)

	got1 := m1.selectEgressNodeID(target, entries)
	reversed := []RouteEntry{entries[1], entries[0]}
	got2 := m2.selectEgressNodeID(target, reversed)

	if got1 == "" {
		t.Fatal("no egress node selected")
	}
	if got1 != got2 {
		t.Fatalf("selection depends on entry order: %q vs %q", got1, got2)
	}
}

// TestMultipleOwnersSameAdvertisedNetwork verifies that two nodes advertising
// the same network through different textual forms collapse into one route
// carrying both owners, and that the admin tree lists both.
func TestMultipleOwnersSameAdvertisedNetwork(t *testing.T) {
	m := newTestMeshManager(t, "self", "100.64.1.0/24", nil)

	// ms9 advertises 10.161.88.12/24; ms10 advertises 10.161.88.0/24. Both
	// normalize to 10.161.88.0/24 but the raw strings differ.
	ms9 := newMockPeerSender("ms9")
	ms10 := newMockPeerSender("ms10")
	m.topology.RegisterPeer(ms9)
	m.topology.RegisterPeer(ms10)
	m.topology.UpdateGossip(ms9, GossipInfo{
		Routes: []GossipRoute{{Prefix: "10.161.88.12/24", NodeID: "ms9"}},
		ClaimedSubnets: []GossipClaimedSubnet{
			{Subnet: "100.64.9.0/24", NodeID: "ms9", Hop: 0},
		},
	})
	m.topology.UpdateGossip(ms10, GossipInfo{
		Routes: []GossipRoute{{Prefix: "10.161.88.0/24", NodeID: "ms10"}},
		ClaimedSubnets: []GossipClaimedSubnet{
			{Subnet: "100.64.10.0/24", NodeID: "ms10", Hop: 0},
		},
	})

	m.recomputeRoutes()

	var matched []MeshRoute
	for _, r := range m.RouteTableRoutes() {
		if r.Prefix.String() == "10.161.88.0/24" {
			matched = append(matched, r)
		}
	}
	if len(matched) != 1 {
		t.Fatalf("got %d routes for 10.161.88.0/24, want 1 (duplicates break first-match stability)", len(matched))
	}
	owners := make([]string, 0, len(matched[0].Entries))
	for _, e := range matched[0].Entries {
		owners = append(owners, e.NodeID)
	}
	sort.Strings(owners)
	if len(owners) != 2 || owners[0] != "ms10" || owners[1] != "ms9" {
		t.Fatalf("route owners = %v, want [ms10 ms9]", owners)
	}

	// The admin tree is built from the trie: it must expose both owners too.
	var findOwners func(nodes []map[string]interface{}) []string
	findOwners = func(nodes []map[string]interface{}) []string {
		for _, n := range nodes {
			if n["full"] == "10.161.88.0/24" {
				raw, _ := n["owners"].([]string)
				return raw
			}
			children, _ := n["children"].([]map[string]interface{})
			if got := findOwners(children); got != nil {
				return got
			}
		}
		return nil
	}
	treeOwners := findOwners(m.GetRouteTree())
	sort.Strings(treeOwners)
	if len(treeOwners) != 2 || treeOwners[0] != "ms10" || treeOwners[1] != "ms9" {
		t.Fatalf("admin tree owners = %v, want [ms10 ms9]", treeOwners)
	}

	// Relayed gossip must preserve both owners of the same prefix.
	info := m.BuildGossipInfo()
	var relayed []string
	for _, r := range info.Routes {
		if r.Prefix == "10.161.88.0/24" {
			relayed = append(relayed, r.NodeID)
		}
	}
	sort.Strings(relayed)
	if len(relayed) != 2 || relayed[0] != "ms10" || relayed[1] != "ms9" {
		t.Fatalf("gossip routes for prefix = %v, want [ms10 ms9]", relayed)
	}
}
