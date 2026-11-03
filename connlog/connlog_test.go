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

func TestCorrelationFields(t *testing.T) {
	resetForTest()

	// Explicit FwdKey wins; srcPort/connId/via pass through.
	Log("TUN", "TCP", "10.99.0.5", "example.com", "198.51.100.1", 443, nil, "ok", nil,
		WithConnID("c-1"), WithSrcPort(5150), WithVia("node-jf"), WithFwdKey("10.99.0.5:40000>10.100.0.7:443"))
	ev := GetLogs()[0]
	if ev.ConnID != "c-1" || ev.SrcPort != 5150 || ev.Via != "node-jf" {
		t.Fatalf("option fields not applied: %+v", ev)
	}
	if ev.FwdKey != "10.99.0.5:40000>10.100.0.7:443" {
		t.Fatalf("explicit FwdKey must win, got %q", ev.FwdKey)
	}

	// Without an explicit key, FwdKey derives from the recorded 5-tuple.
	Log("socks5", "TCP", "192.168.1.10", "", "203.0.113.9", 8080, nil, "ok", nil,
		WithSrcPort(60001))
	ev = GetLogs()[1]
	if ev.FwdKey != "192.168.1.10:60001>203.0.113.9:8080" {
		t.Fatalf("derived FwdKey mismatch: %q", ev.FwdKey)
	}

	// No source port => no meaningful key.
	Log("TUN", "SYSTEM", "", "", "0.0.0.0", 0, nil, "ok", nil)
	if GetLogs()[2].FwdKey != "" {
		t.Fatalf("FwdKey must stay empty without srcPort, got %q", GetLogs()[2].FwdKey)
	}

	// TrackActive: explicit key wins, otherwise derived; RemoveActive journal intact.
	TrackActive("a-1", "TUN", "TCP", "10.99.0.5", "example.com", "198.51.100.1", 443, nil,
		WithActiveSrcPort(5150), WithActiveVia("node-jf"), WithActiveFwdKey("10.99.0.5:40000>10.100.0.7:443"))
	ac := GetActiveConns()[0]
	if ac.FwdKey != "10.99.0.5:40000>10.100.0.7:443" || ac.SrcPort != 5150 || ac.Via != "node-jf" {
		t.Fatalf("active option fields not applied: %+v", ac)
	}
	TrackActive("a-2", "socks5", "TCP", "192.168.1.10", "", "203.0.113.9", 8080, nil,
		WithActiveSrcPort(60001))
	var a2 *ActiveConn
	for i := range GetActiveConns() {
		if GetActiveConns()[i].ID == "a-2" {
			a2 = &GetActiveConns()[i]
		}
	}
	if a2 == nil || a2.FwdKey != "192.168.1.10:60001>203.0.113.9:8080" {
		t.Fatalf("active derived FwdKey mismatch: %+v", a2)
	}

	// Record-level flow: options propagate to both log event and active entry.
	resetForTest()
	rec := Start("TUN", "TCP", "10.99.0.5", "example.com").
		WithConn("r-1", 5150).
		Resolve("198.51.100.1", 443, nil).
		SetVia("node-jf").
		SetFwdKey("10.99.0.5:40000>10.100.0.7:443")
	rec.Establish("r-1", nil, false)
	if GetLogs()[0].FwdKey != "10.99.0.5:40000>10.100.0.7:443" || GetLogs()[0].Via != "node-jf" {
		t.Fatalf("record log fields missing: %+v", GetLogs()[0])
	}
	if GetActiveConns()[0].FwdKey != "10.99.0.5:40000>10.100.0.7:443" {
		t.Fatalf("record active FwdKey missing: %+v", GetActiveConns()[0])
	}
	rec.Close()
	if len(GetActiveConns()) != 0 {
		t.Fatal("Close must remove the active entry")
	}
}

func TestFwdKeyFormat(t *testing.T) {
	if got := FwdKey("1.2.3.4", 1234, "5.6.7.8", 443); got != "1.2.3.4:1234>5.6.7.8:443" {
		t.Fatalf("FwdKey format mismatch: %q", got)
	}
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
