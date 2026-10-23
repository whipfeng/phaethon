package admin

import (
	"strings"
	"sync"
	"time"

	"phaethon/connlog"
	"phaethon/db"
)

// StatsCollector collects runtime statistics for the admin dashboard.
type StatsCollector struct {
	mu sync.RWMutex

	// Counters
	startTime time.Time

	// Listener status
	listenerMu     sync.RWMutex
	listenerStatus map[string]*ListenerStatus

	// Health check status (from ProxyGroup)
	healthMu   sync.RWMutex
	healthData map[string][]*ProxyHealth
}

type ListenerStatus struct {
	Name   string `json:"name"`
	Port   int    `json:"port"`
	Type   string `json:"type"`
	Status string `json:"status"` // "active" | "error" | "stopped"
	Error  string `json:"error,omitempty"`
}

type ProxyHealth struct {
	Name      string        `json:"name"`
	Alive     bool          `json:"alive"`
	Latency   time.Duration `json:"latencyMs"`
	LastCheck time.Time     `json:"lastCheck"`
	FailCount int           `json:"failCount"`
}

func NewStatsCollector() *StatsCollector {
	s := &StatsCollector{
		startTime:      time.Now(),
		listenerStatus: make(map[string]*ListenerStatus),
		healthData:     make(map[string][]*ProxyHealth),
	}
	return s
}

// StartTime returns the process start time.
func (s *StatsCollector) StartTime() time.Time {
	return s.startTime
}

// RegisterListener registers a listener's status.
func (s *StatsCollector) RegisterListener(name string, port int, typ string) {
	s.listenerMu.Lock()
	s.listenerStatus[name] = &ListenerStatus{
		Name:   name,
		Port:   port,
		Type:   typ,
		Status: "active",
	}
	s.listenerMu.Unlock()
}

// SetListenerError marks a listener as errored.
func (s *StatsCollector) SetListenerError(name string, err error) {
	s.listenerMu.Lock()
	if ls, ok := s.listenerStatus[name]; ok {
		ls.Status = "error"
		ls.Error = err.Error()
	}
	s.listenerMu.Unlock()
}

// UpdateHealth updates health check data for a group.
func (s *StatsCollector) UpdateHealth(groupName string, health []*ProxyHealth) {
	s.healthMu.Lock()
	s.healthData[groupName] = health
	s.healthMu.Unlock()
}

// GetSnapshot returns a snapshot of all statistics.
func (s *StatsCollector) GetSnapshot() map[string]interface{} {
	s.listenerMu.RLock()
	listenerCopy := make(map[string]interface{})
	for name, ls := range s.listenerStatus {
		listenerCopy[name] = ls
	}
	s.listenerMu.RUnlock()

	s.healthMu.RLock()
	healthCopy := make(map[string]interface{})
	for name, h := range s.healthData {
		healthCopy[name] = h
	}
	s.healthMu.RUnlock()

	return map[string]interface{}{
		"startTime":         s.startTime.Format(time.RFC3339),
		"totalConnections":  connlog.GetTotalConnections(),
		"activeConnections": connlog.GetActiveCount(),
		"listenerStatus":    listenerCopy,
		"healthData":        healthCopy,
	}
}

// CollectFromConfig extracts health data from database.
func (s *StatsCollector) CollectFromConfig() {
	// Query all health records from database
	allHealth, err := db.ListAllProxyHealth()
	if err != nil {
		return
	}
	
	// Group by group name
	grouped := make(map[string][]*ProxyHealth)
	for _, h := range allHealth {
		health := &ProxyHealth{
			Name:      strings.TrimPrefix(h.ProxyName, "sub:"),
			Alive:     h.Alive,
			Latency:   time.Duration(h.Latency) * time.Millisecond,
			LastCheck: h.LastCheck,
			FailCount: h.FailCount,
		}
		grouped[h.GroupName] = append(grouped[h.GroupName], health)
	}
	
	// Update each group
	for groupName, health := range grouped {
		s.UpdateHealth(groupName, health)
	}
}
