package util

import (
	"encoding/json"
	"fmt"
	"net/http"
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

// VersionNotifier maintains SSE connections and broadcasts version updates.
// Version numbers are stored in the database (db.BucketVersions), not in memory.
type VersionNotifier struct {
	mu            sync.RWMutex
	connections   map[*SSEWriter]struct{}
	heartbeatStop chan struct{}
	getAllVersions func() map[string]interface{} // 从数据库读取所有版本号
}

// NewVersionNotifier creates a new VersionNotifier.
// getAllVersions is a callback to read all versions from the database.
func NewVersionNotifier(getAllVersions func() map[string]interface{}) *VersionNotifier {
	return &VersionNotifier{
		connections:    make(map[*SSEWriter]struct{}),
		getAllVersions: getAllVersions,
	}
}

// BumpVersion sends an incremental update for the specified topic.
// The version number is already stored in the database; this just notifies SSE clients.
func (n *VersionNotifier) BumpVersion(typ string, version uint64) {
	// 只发送变化的 topic
	data, _ := json.Marshal(map[string]uint64{typ: version})
	
	n.mu.RLock()
	conns := make([]*SSEWriter, 0, len(n.connections))
	for c := range n.connections {
		conns = append(conns, c)
	}
	n.mu.RUnlock()
	
	n.broadcast("heartbeat", data, conns)
}

// Subscribe registers a connection. Does not send initial data;
// clients will receive updates via heartbeat events.
func (n *VersionNotifier) Subscribe(conn *SSEWriter) {
	n.mu.Lock()
	n.connections[conn] = struct{}{}
	n.mu.Unlock()
}

// sendHeartbeat sends the current full version vector to a single connection.
func (n *VersionNotifier) sendHeartbeat(conn *SSEWriter) {
	if n.getAllVersions == nil {
		return
	}
	data, _ := json.Marshal(n.getAllVersions())
	conn.WriteEvent("heartbeat", data)
}

// Unsubscribe removes a connection and closes it.
func (n *VersionNotifier) Unsubscribe(conn *SSEWriter) {
	n.mu.Lock()
	delete(n.connections, conn)
	n.mu.Unlock()
	conn.Close()
}

// Versions returns the current version vector from the database.
func (n *VersionNotifier) Versions() map[string]interface{} {
	if n.getAllVersions == nil {
		return make(map[string]interface{})
	}
	return n.getAllVersions()
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
	if n.getAllVersions == nil {
		return
	}
	data, _ := json.Marshal(n.getAllVersions())
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

// DefaultVersionNotifier is the process-wide version notifier used by the
// admin panel. It will be initialized with a database callback in db.InitVersionNotifier.
var DefaultVersionNotifier *VersionNotifier

func init() {
	// Temporary initialization without database callback.
	// Will be replaced in db.InitVersionNotifier() with proper callback.
	DefaultVersionNotifier = NewVersionNotifier(nil)
}
