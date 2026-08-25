package util

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// SSEWriter wraps an http.ResponseWriter for Server-Sent Events output.
// It is safe for concurrent use.
type SSEWriter struct {
	mu     sync.Mutex
	w      http.ResponseWriter
	f      http.Flusher
	closed bool
}

// NewSSEWriter creates an SSE writer for w. It returns nil if w does not
// support http.Flusher.
func NewSSEWriter(w http.ResponseWriter) *SSEWriter {
	f, ok := w.(http.Flusher)
	if !ok {
		return nil
	}
	return &SSEWriter{w: w, f: f}
}

// WriteEvent writes a single SSE event. It returns false if the writer is
// closed or the underlying write fails.
func (s *SSEWriter) WriteEvent(event string, data []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		s.closed = true
		return false
	}
	s.f.Flush()
	return true
}

// Close marks the writer as closed so future writes are ignored.
func (s *SSEWriter) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
}

// VersionNotifier maintains an in-memory version table and a set of SSE
// connections. All fields are protected by mu; versions and connections are
// intentionally not persisted across restarts.
type VersionNotifier struct {
	mu            sync.RWMutex
	versions      map[string]uint64
	connections   map[*SSEWriter]struct{}
	bootTime      string
	heartbeatStop chan struct{}
}

// NewVersionNotifier creates a new, empty VersionNotifier.
func NewVersionNotifier() *VersionNotifier {
	return &VersionNotifier{
		versions:    make(map[string]uint64),
		connections: make(map[*SSEWriter]struct{}),
		bootTime:    strconv.FormatInt(time.Now().UnixNano(), 10),
	}
}

// BumpVersion increments the version for typ and broadcasts the full version
// vector as a heartbeat to all connected clients. It returns the new version
// number.
func (n *VersionNotifier) BumpVersion(typ string) uint64 {
	n.mu.Lock()
	n.versions[typ]++
	v := n.versions[typ]
	n.mu.Unlock()

	n.broadcastSnapshot()
	return v
}

// Subscribe registers a connection and immediately sends the current full
// version vector as a heartbeat.
func (n *VersionNotifier) Subscribe(conn *SSEWriter) {
	n.mu.Lock()
	n.connections[conn] = struct{}{}
	n.mu.Unlock()

	n.sendHeartbeat(conn)
}

// sendHeartbeat sends the current full version vector to a single connection.
func (n *VersionNotifier) sendHeartbeat(conn *SSEWriter) {
	data, _ := json.Marshal(n.Versions())
	conn.WriteEvent("heartbeat", data)
}

// Unsubscribe removes a connection and closes it.
func (n *VersionNotifier) Unsubscribe(conn *SSEWriter) {
	n.mu.Lock()
	delete(n.connections, conn)
	n.mu.Unlock()
	conn.Close()
}

// Versions returns a copy of the current version vector plus the server boot
// time under the key "_bootTime". The boot time is an opaque string that
// changes on every process restart; clients should compare it for equality
// only.
func (n *VersionNotifier) Versions() map[string]interface{} {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.snapshotLocked()
}

// BootTime returns the opaque server boot time string.
func (n *VersionNotifier) BootTime() string {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.bootTime
}

// StartHeartbeat starts a goroutine that broadcasts the full version vector
// (heartbeat) to all connections every interval. It is safe to call multiple
// times.
func (n *VersionNotifier) StartHeartbeat(interval time.Duration) {
	n.mu.Lock()
	if n.heartbeatStop != nil {
		n.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	n.heartbeatStop = stop
	n.mu.Unlock()

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				n.broadcastSnapshot()
			case <-stop:
				return
			}
		}
	}()
}

// StopHeartbeat stops the periodic snapshot goroutine.
func (n *VersionNotifier) StopHeartbeat() {
	n.mu.Lock()
	if n.heartbeatStop != nil {
		close(n.heartbeatStop)
		n.heartbeatStop = nil
	}
	n.mu.Unlock()
}

func (n *VersionNotifier) broadcastSnapshot() {
	data, _ := json.Marshal(n.Versions())
	n.mu.RLock()
	conns := make([]*SSEWriter, 0, len(n.connections))
	for c := range n.connections {
		conns = append(conns, c)
	}
	n.mu.RUnlock()
	n.broadcast("heartbeat", data, conns)
}

func (n *VersionNotifier) broadcast(event string, data []byte, conns []*SSEWriter) {
	for _, c := range conns {
		if !c.WriteEvent(event, data) {
			n.Unsubscribe(c)
		}
	}
}

func (n *VersionNotifier) connectionsLocked() []*SSEWriter {
	conns := make([]*SSEWriter, 0, len(n.connections))
	for c := range n.connections {
		conns = append(conns, c)
	}
	return conns
}

func (n *VersionNotifier) snapshotLocked() map[string]interface{} {
	out := make(map[string]interface{}, len(n.versions)+1)
	for k, v := range n.versions {
		out[k] = v
	}
	out["_bootTime"] = n.bootTime
	return out
}

// DefaultVersionNotifier is the process-wide version notifier used by the
// admin panel.
var DefaultVersionNotifier = NewVersionNotifier()
