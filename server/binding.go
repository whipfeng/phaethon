package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"phaethon/util"
)

const bindingFileName = "bindings.json"

// PortBinding records the mapping between a client identity and its allocated port.
type PortBinding struct {
	ReverseID      string    `json:"reverse_id"`
	Seq            int       `json:"seq"`
	Port           int       `json:"port"`
	ListenerProto  string    `json:"listener_proto"`
	Identity       string    `json:"identity"` // fallback: proto|listener|user
	DirectDstHost  string    `json:"direct_dst_host,omitempty"`
	DirectDstPort  int       `json:"direct_dst_port,omitempty"`
	RegistryProxy  string    `json:"registry_proxy,omitempty"` // proxy used to reach the registry
	ControlAddr    string    `json:"control_addr"`             // registry-known source address of the control connection
	RegistryAddr   string    `json:"registry_addr"`            // registry-side local address the control connection arrived on
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	DisconnectedAt time.Time `json:"disconnected_at"` // set by HandleClose when control channel drops
}

// BindingStore holds all port bindings and provides persistence.
type BindingStore struct {
	mu       sync.RWMutex
	bindings map[string]*PortBinding // key: fmt.Sprintf("%s#%d", reverse_id, seq)
	dataDir  string
}

// NewBindingStore creates a binding store that persists to <dataDir>/bindings.json.
func NewBindingStore(dataDir string) *BindingStore {
	s := &BindingStore{
		bindings: make(map[string]*PortBinding),
		dataDir:  dataDir,
	}
	s.load()
	return s
}

// makeKey returns the composite key for a reverseID + seq pair.
func makeKey(reverseID string, seq int) string {
	return fmt.Sprintf("%s#%d", reverseID, seq)
}

// Snapshot returns a defensive copy of all bindings, sorted by UpdatedAt
// descending (most recently active first).
func (s *BindingStore) Snapshot() []PortBinding {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := make([]PortBinding, 0, len(s.bindings))
	for _, b := range s.bindings {
		list = append(list, *b)
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].UpdatedAt.After(list[j].UpdatedAt)
	})
	return list
}

// Get returns the binding for a client, or nil if not found.
func (s *BindingStore) Get(reverseID string, seq int) *PortBinding {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.bindings[makeKey(reverseID, seq)]
}

// Set records or updates a binding for a client.
func (s *BindingStore) Set(reverseID string, seq int, port int, listenerProto, identity, directDstHost string, directDstPort int, registryProxy string, controlAddr string, registryAddr string) {
	s.mu.Lock()
	key := makeKey(reverseID, seq)
	now := time.Now()
	if b, ok := s.bindings[key]; ok {
		b.Port = port
		b.ListenerProto = listenerProto
		b.Identity = identity
		b.DirectDstHost = directDstHost
		b.DirectDstPort = directDstPort
		b.RegistryProxy = registryProxy
		b.ControlAddr = controlAddr
		b.RegistryAddr = registryAddr
		b.UpdatedAt = now
		b.DisconnectedAt = time.Time{}
	} else {
		s.bindings[key] = &PortBinding{
			ReverseID:     reverseID,
			Seq:           seq,
			Port:          port,
			ListenerProto: listenerProto,
			Identity:      identity,
			DirectDstHost: directDstHost,
			DirectDstPort: directDstPort,
			RegistryProxy: registryProxy,
			ControlAddr:   controlAddr,
			RegistryAddr:  registryAddr,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
	}
	s.saveLocked()
	s.mu.Unlock()

	// Publish outside the lock — Snapshot() acquires its own RLock and would
	// deadlock if called while we still hold the write lock.
	s.publishSnapshot()
}

// Remove deletes a binding.
func (s *BindingStore) Remove(reverseID string, seq int) {
	s.mu.Lock()
	delete(s.bindings, makeKey(reverseID, seq))
	s.saveLocked()
	s.mu.Unlock()

	// Publish outside the lock (same reason as Set).
	s.publishSnapshot()
}

// IsPortBoundByOther checks if the given port is already bound to a different
// client (not excludeReverseID+excludeSeq). Used by allocatePort to avoid reassigning a
// port that belongs to another (possibly offline) reverse client.
func (s *BindingStore) IsPortBoundByOther(port int, excludeReverseID string, excludeSeq int) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for key, b := range s.bindings {
		if b.Port == port {
			if key != makeKey(excludeReverseID, excludeSeq) {
				return true
			}
		}
	}
	return false
}

// SetDisconnected records the disconnect timestamp for a client's binding.
func (s *BindingStore) SetDisconnected(reverseID string, seq int) {
	s.mu.Lock()
	if b, ok := s.bindings[makeKey(reverseID, seq)]; ok {
		b.DisconnectedAt = time.Now()
		s.saveLocked()
	}
	s.mu.Unlock()

	// Publish outside the lock (same reason as Set).
	s.publishSnapshot()
}

// publishSnapshot bumps the bindings topic version so connected browsers know
// to fetch the latest binding list via REST.
func (s *BindingStore) publishSnapshot() {
	util.DefaultVersionNotifier.BumpVersion("bindings")
}

// FindBindingByPort returns the binding that owns the given port, or nil.
func (s *BindingStore) FindBindingByPort(port int) *PortBinding {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, b := range s.bindings {
		if b.Port == port {
			return b
		}
	}
	return nil
}

// FindOldestDisconnectedBinding returns the single binding within [min,max]
// that has the oldest (earliest) DisconnectedAt. Returns nil if no
// disconnected binding exists in the range.
func (s *BindingStore) FindOldestDisconnectedBinding(portMin, portMax int) *PortBinding {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var oldest *PortBinding
	for _, b := range s.bindings {
		if b.Port < portMin || b.Port > portMax || b.DisconnectedAt.IsZero() {
			continue
		}
		if oldest == nil || b.DisconnectedAt.Before(oldest.DisconnectedAt) {
			oldest = b
		}
	}
	return oldest
}

// bindingFilePath returns the full path to the bindings file.
func (s *BindingStore) bindingFilePath() string {
	return filepath.Join(s.dataDir, bindingFileName)
}

// load reads bindings from disk.
func (s *BindingStore) load() {
	path := s.bindingFilePath()
	data, err := os.ReadFile(path)
	if err != nil {
		return // file not exist is ok
	}
	var list []PortBinding
	if err := json.Unmarshal(data, &list); err != nil {
		return // corrupt file, start fresh
	}
	for i := range list {
		b := &list[i]
		key := makeKey(b.ReverseID, b.Seq)
		// Migrate any legacy reverse_id-only keys (pre-Seq format) to the new composite key.
		if b.Seq == 0 && b.ReverseID != "" {
			delete(s.bindings, b.ReverseID)
		}
		s.bindings[key] = b
	}
}

// saveLocked writes bindings to disk. Must be called with mu held.
func (s *BindingStore) saveLocked() {
	list := make([]PortBinding, 0, len(s.bindings))
	for _, b := range s.bindings {
		list = append(list, *b)
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return
	}
	path := s.bindingFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0644)
}
