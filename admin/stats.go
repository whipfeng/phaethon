package admin

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"phaethon/config"
	"phaethon/connlog"
	"phaethon/util"
)

// StatsCollector collects runtime statistics for the admin dashboard.
type StatsCollector struct {
	mu sync.RWMutex

	// Counters
	startTime         time.Time
	totalConnections  atomic.Int64
	activeConnections atomic.Int64

	// Traffic by proxy name (bytes)
	trafficMu      sync.Mutex
	trafficByProxy map[string]*TrafficStats

	// Listener status
	listenerMu     sync.RWMutex
	listenerStatus map[string]*ListenerStatus

	// Health check status (from ProxyGroup)
	healthMu   sync.RWMutex
	healthData map[string][]*ProxyHealth

	// Debounce for version bumping
	bumpMu    sync.Mutex
	bumpTimer *time.Timer
}

type TrafficStats struct {
	BytesUp    int64     `json:"bytesUp"`
	BytesDown  int64     `json:"bytesDown"`
	ConnCount  int64     `json:"connCount"`
	LastActive time.Time `json:"lastActive"`
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
		trafficByProxy: make(map[string]*TrafficStats),
		listenerStatus: make(map[string]*ListenerStatus),
		healthData:     make(map[string][]*ProxyHealth),
	}
	return s
}

// StartTime returns the process start time.
func (s *StatsCollector) StartTime() time.Time {
	return s.startTime
}

// OnConnect records a new connection.
func (s *StatsCollector) OnConnect(proxyName string) {
	s.totalConnections.Add(1)
	s.activeConnections.Add(1)

	s.trafficMu.Lock()
	if _, ok := s.trafficByProxy[proxyName]; !ok {
		s.trafficByProxy[proxyName] = &TrafficStats{}
	}
	ts := s.trafficByProxy[proxyName]
	ts.ConnCount++
	ts.LastActive = time.Now()
	s.trafficMu.Unlock()

	util.DefaultVersionNotifier.BumpVersion("stats")
}

// OnDisconnect records a connection close.
func (s *StatsCollector) OnDisconnect() {
	s.activeConnections.Add(-1)
	util.DefaultVersionNotifier.BumpVersion("stats")
}

// RecordTraffic records bytes transferred for a proxy.
func (s *StatsCollector) RecordTraffic(proxyName string, up, down int64) {
	s.trafficMu.Lock()
	if _, ok := s.trafficByProxy[proxyName]; !ok {
		s.trafficByProxy[proxyName] = &TrafficStats{}
	}
	ts := s.trafficByProxy[proxyName]
	ts.BytesUp += up
	ts.BytesDown += down
	ts.LastActive = time.Now()
	s.trafficMu.Unlock()
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
	s.trafficMu.Lock()
	trafficCopy := make(map[string]interface{})
	for name, ts := range s.trafficByProxy {
		trafficCopy[name] = map[string]interface{}{
			"bytesUp":    ts.BytesUp,
			"bytesDown":  ts.BytesDown,
			"connCount":  ts.ConnCount,
			"lastActive": ts.LastActive.Format(time.RFC3339),
		}
	}
	s.trafficMu.Unlock()

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
		"totalConnections":  s.totalConnections.Load(),
		"activeConnections": connlog.GetActiveCount(),
		"trafficByProxy":    trafficCopy,
		"listenerStatus":    listenerCopy,
		"healthData":        healthCopy,
	}
}

// CollectFromConfig extracts health data from RuleConfiguration.
func (s *StatsCollector) CollectFromConfig(conf *config.RuleConfiguration) {
	for _, g := range conf.ProxyGroups {
		snapshot := g.HealthSnapshot()
		if len(snapshot) == 0 {
			continue
		}
		var health []*ProxyHealth
		for name, hi := range snapshot {
			health = append(health, &ProxyHealth{
				Name:      strings.TrimPrefix(name, "sub:"),
				Alive:     hi.Alive,
				Latency:   hi.Latency,
				LastCheck: hi.LastCheck,
				FailCount: hi.FailCount,
			})
		}
		s.UpdateHealth(g.Name, health)
	}
}
