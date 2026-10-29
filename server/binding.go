package server

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"phaethon/db"
	"phaethon/util"
)

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
}

// Remove deletes a binding.
func (s *BindingStore) Remove(reverseID string, seq int) {
	s.mu.Lock()
	delete(s.bindings, makeKey(reverseID, seq))
	s.saveLocked()
	s.mu.Unlock()
}

// RemoveByReverseID removes all bindings for the given reverseID, regardless of seq.
// This handles client restart where the seq may have changed.
func (s *BindingStore) RemoveByReverseID(reverseID string) {
	if reverseID == "" {
		return
	}
	s.mu.Lock()
	for key, b := range s.bindings {
		if b.ReverseID == reverseID {
			delete(s.bindings, key)
		}
	}
	s.saveLocked()
	s.mu.Unlock()
}

// IsPortBoundByOther checks if the given port is already bound to a different
// client. A port is considered "bound by other" only if the reverseID differs;
// the same reverseID can reclaim its ports regardless of seq (handles restart).
func (s *BindingStore) IsPortBoundByOther(port int, excludeReverseID string, excludeSeq int) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, b := range s.bindings {
		if b.Port == port {
			if b.ReverseID != excludeReverseID {
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

// load reads bindings from database.
func (s *BindingStore) load() {
	dbBindings, err := db.GetPortBindings()
	if err != nil {
		util.LogWarn("load bindings from database failed: %v", err)
		return
	}
	for i := range dbBindings {
		b := &dbBindings[i]
		// Convert db.PortBinding to server.PortBinding
		serverBinding := &PortBinding{
			ReverseID:      b.ReverseID,
			Seq:            b.Seq,
			Port:           b.Port,
			ListenerProto:  b.ListenerProto,
			Identity:       b.Identity,
			DirectDstHost:  b.DirectDstHost,
			DirectDstPort:  b.DirectDstPort,
			RegistryProxy:  b.RegistryProxy,
			ControlAddr:    b.ControlAddr,
			RegistryAddr:   b.RegistryAddr,
			CreatedAt:      b.CreatedAt,
			UpdatedAt:      b.UpdatedAt,
			DisconnectedAt: b.DisconnectedAt,
		}
		key := makeKey(serverBinding.ReverseID, serverBinding.Seq)
		s.bindings[key] = serverBinding
	}
	util.LogInfo("bindings loaded from database: %d entries", len(dbBindings))
}

// saveLocked writes bindings to database. Must be called with mu held.
func (s *BindingStore) saveLocked() {
	list := make([]db.PortBinding, 0, len(s.bindings))
	for _, b := range s.bindings {
		// Convert server.PortBinding to db.PortBinding
		list = append(list, db.PortBinding{
			ReverseID:      b.ReverseID,
			Seq:            b.Seq,
			Port:           b.Port,
			ListenerProto:  b.ListenerProto,
			Identity:       b.Identity,
			DirectDstHost:  b.DirectDstHost,
			DirectDstPort:  b.DirectDstPort,
			RegistryProxy:  b.RegistryProxy,
			ControlAddr:    b.ControlAddr,
			RegistryAddr:   b.RegistryAddr,
			CreatedAt:      b.CreatedAt,
			UpdatedAt:      b.UpdatedAt,
			DisconnectedAt: b.DisconnectedAt,
		})
	}
	if err := db.PutPortBindings(list); err != nil {
		util.LogWarn("save bindings to database failed: %v", err)
	}
}
