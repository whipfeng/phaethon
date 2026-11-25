package mesh

import (
	"net"
	"testing"
	"time"
)

func TestStickyLRUPinnedAcrossGenerationChange(t *testing.T) {
	c := newStickyLRU(4)

	c.Put("10.0.0.1", "ms9", 1)
	if got, ok := c.Get("10.0.0.1", 1, stickyRebalanceGrace); !ok || got != "ms9" {
		t.Fatalf("same generation: got %q ok=%v", got, ok)
	}

	// Recently used: a new topology generation must not move it.
	if got, ok := c.Get("10.0.0.1", 2, stickyRebalanceGrace); !ok || got != "ms9" {
		t.Fatalf("generation change within grace: got %q ok=%v, want pinned", got, ok)
	}
}

func TestStickyLRUExpiresIdleAfterGenerationChange(t *testing.T) {
	c := newStickyLRU(4)
	c.Put("10.0.0.1", "ms9", 1)

	idle := func() {
		c.mu.Lock()
		c.items["10.0.0.1"].Value.(*stickyLRUEntry).lastUsed = time.Now().Add(-2 * stickyRebalanceGrace)
		c.mu.Unlock()
	}

	// Same generation: idle time alone never expires the entry.
	idle()
	if _, ok := c.Get("10.0.0.1", 1, stickyRebalanceGrace); !ok {
		t.Fatal("same generation expired on idle time")
	}

	// Stale generation + idle past grace → miss and removal.
	idle()
	if got, ok := c.Get("10.0.0.1", 2, stickyRebalanceGrace); ok {
		t.Fatalf("stale generation after grace: got %q, want miss", got)
	}
	c.mu.Lock()
	_, stillThere := c.items["10.0.0.1"]
	c.mu.Unlock()
	if stillThere {
		t.Fatal("expired entry was not removed")
	}
}

func TestStickyLRUCapacityEviction(t *testing.T) {
	c := newStickyLRU(2)
	c.Put("a", "n1", 1)
	c.Put("b", "n2", 1)
	c.Put("c", "n3", 1)

	if _, ok := c.Get("a", 1, stickyRebalanceGrace); ok {
		t.Fatal("oldest entry should be evicted")
	}
	if _, ok := c.Get("c", 1, stickyRebalanceGrace); !ok {
		t.Fatal("newest entry missing")
	}
}

// TestSelectEgressNodeIDStickyGeneration verifies the manager-level wiring:
// an active selection survives a rebuild, an idle one is re-selected under the
// new generation.
func TestSelectEgressNodeIDStickyGeneration(t *testing.T) {
	target := net.ParseIP("10.161.88.9")
	entries := []RouteEntry{
		{NodeID: "ms9", Source: RouteSourceDynamic},
		{NodeID: "ms10", Source: RouteSourceDynamic},
	}
	m := newTestMeshManager(t, "self", "100.64.1.0/24", nil)

	m.sticky.Put(target.String(), "ms9", m.routeGen.Load())
	m.routeGen.Add(1) // topology rebuild

	if got := m.selectEgressNodeID(target, entries); got != "ms9" {
		t.Fatalf("active sticky selection moved on rebuild: got %q, want ms9", got)
	}

	m.sticky.mu.Lock()
	m.sticky.items[target.String()].Value.(*stickyLRUEntry).lastUsed = time.Now().Add(-2 * stickyRebalanceGrace)
	m.sticky.mu.Unlock()
	m.routeGen.Add(1)

	got := m.selectEgressNodeID(target, entries)
	if got != "ms9" && got != "ms10" {
		t.Fatalf("re-selected unknown owner %q", got)
	}
	m.sticky.mu.Lock()
	entry, ok := m.sticky.items[target.String()]
	var entryGen uint64
	if ok {
		entryGen = entry.Value.(*stickyLRUEntry).gen
	}
	m.sticky.mu.Unlock()
	if !ok || entryGen != m.routeGen.Load() {
		t.Fatalf("re-selection not recorded under current generation (ok=%v gen=%d want %d)", ok, entryGen, m.routeGen.Load())
	}
}

func newWeightedTestMesh(t *testing.T, costs map[string]float64) *MeshManager {
	t.Helper()
	m := newTestMeshManager(t, "self", "100.64.1.0/24", nil)
	paths := make(map[string]dijkstraResult, len(costs))
	for node, cost := range costs {
		paths[node] = dijkstraResult{Cost: cost}
	}
	m.routeTable.Store(&routeTable{gen: m.routeGen.Add(1), bestPaths: paths})
	return m
}

// TestPickWeightedOwnerPrefersCheaperPath verifies weight = 1/cost: with costs
// 1 vs 10 the cheap owner should take roughly 10x the traffic.
func TestPickWeightedOwnerPrefersCheaperPath(t *testing.T) {
	m := newWeightedTestMesh(t, map[string]float64{"cheap": 1, "dear": 10})
	entries := []RouteEntry{
		{NodeID: "cheap", Source: RouteSourceDynamic},
		{NodeID: "dear", Source: RouteSourceDynamic},
	}
	counts := map[string]int{}
	for i := 0; i < 2000; i++ {
		counts[m.selectEgressNodeID(net.IPv4(10, 1, byte(i>>8), byte(i)), entries)]++
	}
	if counts["cheap"] <= counts["dear"]*4 {
		t.Fatalf("weighting ineffective, want cheap >> dear: %v", counts)
	}
}

// TestPickWeightedOwnerUniformWhenEqualCost verifies equal costs degrade to an
// even spread.
func TestPickWeightedOwnerUniformWhenEqualCost(t *testing.T) {
	m := newWeightedTestMesh(t, map[string]float64{"a": 3, "b": 3})
	entries := []RouteEntry{
		{NodeID: "a", Source: RouteSourceDynamic},
		{NodeID: "b", Source: RouteSourceDynamic},
	}
	counts := map[string]int{}
	for i := 0; i < 2000; i++ {
		counts[m.selectEgressNodeID(net.IPv4(10, 2, byte(i>>8), byte(i)), entries)]++
	}
	for _, node := range []string{"a", "b"} {
		if counts[node] < 400 || counts[node] > 1600 {
			t.Fatalf("equal-cost spread out of range: %v", counts)
		}
	}
}
