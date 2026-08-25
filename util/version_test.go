package util

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestVersionNotifier_BumpVersionBroadcastsUpdate(t *testing.T) {
	n := NewVersionNotifier()

	// Capture two SSE connections.
	rec1 := httptest.NewRecorder()
	rec2 := httptest.NewRecorder()
	w1 := NewSSEWriter(rec1)
	w2 := NewSSEWriter(rec2)
	if w1 == nil || w2 == nil {
		t.Fatal("ResponseRecorder does not support Flusher")
	}

	n.Subscribe(w1)
	n.Subscribe(w2)

	// Bump a version and broadcast.
	v := n.BumpVersion("config")
	if v != 1 {
		t.Fatalf("expected version 1, got %d", v)
	}

	// Give the goroutine-less broadcast a moment to write.
	time.Sleep(50 * time.Millisecond)

	want := `event: heartbeat`
	wantData := `"config":1`
	for i, rec := range []*httptest.ResponseRecorder{rec1, rec2} {
		got := rec.Body.String()
		if !strings.Contains(got, want) {
			t.Fatalf("connection %d body missing heartbeat event:\n%s", i+1, got)
		}
		if !strings.Contains(got, wantData) {
			t.Fatalf("connection %d body missing config version:\n%s", i+1, got)
		}
		if !strings.Contains(got, `"_bootTime":`) {
			t.Fatalf("connection %d body missing _bootTime:\n%s", i+1, got)
		}
	}
}

func TestVersionNotifier_SubscribeSendsSnapshot(t *testing.T) {
	n := NewVersionNotifier()
	n.BumpVersion("product")
	n.BumpVersion("product")
	n.BumpVersion("order")

	rec := httptest.NewRecorder()
	w := NewSSEWriter(rec)
	if w == nil {
		t.Fatal("ResponseRecorder does not support Flusher")
	}

	n.Subscribe(w)

	got := rec.Body.String()
	if !strings.Contains(got, "event: heartbeat") {
		t.Fatalf("missing heartbeat event: %s", got)
	}
	if !strings.Contains(got, `"product":2`) {
		t.Fatalf("missing product version: %s", got)
	}
	if !strings.Contains(got, `"order":1`) {
		t.Fatalf("missing order version: %s", got)
	}
}

func TestVersionNotifier_UnsubscribeRemovesConnection(t *testing.T) {
	n := NewVersionNotifier()
	rec := httptest.NewRecorder()
	w := NewSSEWriter(rec)
	if w == nil {
		t.Fatal("ResponseRecorder does not support Flusher")
	}

	n.Subscribe(w)
	n.Unsubscribe(w)

	n.mu.RLock()
	_, ok := n.connections[w]
	n.mu.RUnlock()
	if ok {
		t.Fatal("connection should have been removed")
	}
}

func TestSSEWriter_WriteEventFormat(t *testing.T) {
	rec := httptest.NewRecorder()
	w := NewSSEWriter(rec)
	if w == nil {
		t.Fatal("ResponseRecorder does not support Flusher")
	}

	if !w.WriteEvent("heartbeat", []byte(`{"type":"x","version":5}`)) {
		t.Fatal("WriteEvent returned false")
	}

	got := rec.Body.String()
	want := "event: heartbeat\ndata: {\"type\":\"x\",\"version\":5}\n\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSSEWriter_WriteEventFailsWhenClosed(t *testing.T) {
	rec := httptest.NewRecorder()
	w := NewSSEWriter(rec)
	if w == nil {
		t.Fatal("ResponseRecorder does not support Flusher")
	}
	w.Close()
	if w.WriteEvent("heartbeat", []byte(`{}`)) {
		t.Fatal("WriteEvent should return false on closed writer")
	}
}

func TestVersionNotifier_HeartbeatBroadcastsSnapshot(t *testing.T) {
	n := NewVersionNotifier()
	rec := httptest.NewRecorder()
	w := NewSSEWriter(rec)
	if w == nil {
		t.Fatal("ResponseRecorder does not support Flusher")
	}

	n.Subscribe(w)
	n.BumpVersion("product")

	// Reset body and start a short heartbeat.
	rec.Body.Reset()
	n.StartHeartbeat(50 * time.Millisecond)
	defer n.StopHeartbeat()

	time.Sleep(80 * time.Millisecond)

	got := rec.Body.String()
	if !strings.Contains(got, "event: heartbeat") {
		t.Fatalf("missing heartbeat: %s", got)
	}
	if !strings.Contains(got, `"product":1`) {
		t.Fatalf("missing product version in heartbeat: %s", got)
	}
}

// nonFlusherRecorder is an http.ResponseWriter without Flusher.
type nonFlusherRecorder struct {
	body   *bytes.Buffer
	header http.Header
}

func (n *nonFlusherRecorder) Header() http.Header         { return n.header }
func (n *nonFlusherRecorder) Write(p []byte) (int, error) { return n.body.Write(p) }
func (n *nonFlusherRecorder) WriteHeader(int)             {}

// Ensure NewSSEWriter requires a Flusher.
func TestNewSSEWriter_RequiresFlusher(t *testing.T) {
	plain := &nonFlusherRecorder{body: new(bytes.Buffer), header: make(http.Header)}
	if NewSSEWriter(plain) != nil {
		t.Fatal("expected nil for non-Flusher ResponseWriter")
	}

	flush := httptest.NewRecorder()
	if NewSSEWriter(flush) == nil {
		t.Fatal("expected non-nil for Flusher ResponseWriter")
	}
}

// brokenWriter fails on every Write.
type brokenWriter struct {
	headers http.Header
}

func (b *brokenWriter) Header() http.Header { return b.headers }
func (b *brokenWriter) Write([]byte) (int, error) {
	return 0, fmt.Errorf("intentional write error")
}
func (b *brokenWriter) WriteHeader(int) {}
func (b *brokenWriter) Flush()          {}

func TestSSEWriter_WriteEventReturnsFalseOnError(t *testing.T) {
	w := NewSSEWriter(&brokenWriter{headers: make(http.Header)})
	if w.WriteEvent("heartbeat", []byte(`{}`)) {
		t.Fatal("expected false on write error")
	}
}

func TestVersionNotifier_DeadConnectionRemovedDuringBroadcast(t *testing.T) {
	n := NewVersionNotifier()
	good := httptest.NewRecorder()
	bad := &brokenWriter{headers: make(http.Header)}

	wGood := NewSSEWriter(good)
	wBad := NewSSEWriter(bad)

	n.Subscribe(wGood)
	n.Subscribe(wBad)

	n.BumpVersion("x")

	n.mu.RLock()
	_, badStillThere := n.connections[wBad]
	n.mu.RUnlock()
	if badStillThere {
		t.Fatal("dead connection should have been removed")
	}

	// Good connection should still receive the event.
	if !bytes.Contains(good.Body.Bytes(), []byte(`"x":1`)) {
		t.Fatalf("good connection missed event: %s", good.Body.String())
	}
}
