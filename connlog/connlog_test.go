package connlog

import (
	"sync"
	"testing"
)

// resetForTest simulates a process restart: in-memory counters and
// journals are wiped while a client somewhere still holds an old cursor.
func resetForTest() {
	mu.Lock()
	logs = nil
	nextSeq = 1
	totalConnections = 0
	mu.Unlock()
	version.Store(0)
	journal = nil
	activeMap = sync.Map{}
}

func TestGetLogsAfterSeqNormal(t *testing.T) {
	resetForTest()
	Log("in", "TCP", "s", "", "d", 80, nil, "ok", nil)
	Log("in", "TCP", "s", "", "d", 81, nil, "ok", nil)

	events, stale := GetLogsAfterSeq(0)
	if stale {
		t.Fatal("fresh cursor should not be stale")
	}
	if len(events) != 2 {
		t.Fatalf("want 2 events, got %d", len(events))
	}

	events, stale = GetLogsAfterSeq(uint64(events[0].Seq))
	if stale || len(events) != 1 {
		t.Fatalf("incremental want [1 ev] stale=false, got %d stale=%v", len(events), stale)
	}
}

func TestGetLogsAfterSeqCounterReset(t *testing.T) {
	resetForTest()
	Log("in", "TCP", "s", "", "d", 80, nil, "ok", nil)
	Log("in", "TCP", "s", "", "d", 81, nil, "ok", nil)
	oldCursor := version.Load()

	resetForTest() // process restart
	Log("in", "TCP", "s", "", "e", 443, nil, "ok", nil)

	events, stale := GetLogsAfterSeq(oldCursor)
	if !stale {
		t.Fatal("cursor from before restart must be flagged stale")
	}
	if len(events) != 1 || events[0].DstPort != 443 {
		t.Fatalf("stale snapshot must contain only post-restart logs, got %+v", events)
	}
}

func TestGetActiveConnsAfterSeqNormal(t *testing.T) {
	resetForTest()
	TrackActive("c1", "TUN", "TCP", "s", "", "d", 80, nil)
	cursor := version.Load() // 1
	TrackActive("c2", "TUN", "TCP", "s", "", "d", 81, nil)
	RemoveActive("c1")

	entries, conns, stale := GetActiveConnsAfterSeq(cursor)
	if stale {
		t.Fatal("live cursor within journal window should not be stale")
	}
	if len(conns) != 0 {
		t.Fatalf("incremental response must not carry a full snapshot, got %d conns", len(conns))
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 journal entries after cursor, got %d", len(entries))
	}
	if len(GetActiveConns()) != 1 {
		t.Fatal("active map should hold c2 only")
	}
}

func TestGetActiveConnsAfterSeqCounterReset(t *testing.T) {
	resetForTest()
	TrackActive("old1", "TUN", "TCP", "s", "", "d", 80, nil)
	TrackActive("old2", "TUN", "TCP", "s", "", "d", 81, nil)
	TrackActive("old3", "TUN", "TCP", "s", "", "d", 82, nil)
	oldCursor := version.Load() // 3

	resetForTest() // process restart
	TrackActive("new1", "TUN", "TCP", "s", "", "e", 443, nil)

	// Cursor strictly newer than anything this process issued => reset.
	entries, conns, stale := GetActiveConnsAfterSeq(oldCursor)
	if !stale {
		t.Fatal("cursor from before restart must be flagged stale")
	}
	if len(entries) != 0 {
		t.Fatalf("stale response must not carry journal entries, got %d", len(entries))
	}
	if len(conns) != 1 || conns[0].ID != "new1" {
		t.Fatalf("stale snapshot must hold only post-restart conns, got %+v", conns)
	}

	// Equality zone (cursor <= current version after restart) is
	// indistinguishable from a live cursor by seq alone; the API's boot
	// field covers it — the seq guard must NOT claim it stale, otherwise
	// every ordinary incremental fetch in that window would full-sync.
	entries, _, stale = GetActiveConnsAfterSeq(1)
	if stale {
		t.Fatal("live cursor in overlap window must stay incremental (boot field covers exact restart detection)")
	}
	_ = entries
}
