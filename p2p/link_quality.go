package p2p

import (
	"math"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/protocol/ping"

	"phaethon/util"
)

// LinkQualityMonitor monitors link quality for each connection.
type LinkQualityMonitor struct {
	p2pMgr  *P2PManager
	localID string

	mu sync.RWMutex

	// Link quality by connection ID
	links map[string]*LinkMetrics // connID -> metrics

	// Configuration
	probeInterval time.Duration
	probeTimeout  time.Duration

	stopCh chan struct{}
}

// LinkMetrics contains metrics for a single link.
type LinkMetrics struct {
	// Direct measurements
	RTT        float64 // Average RTT (ms)
	RTTMin     float64 // Minimum RTT
	RTTMax     float64 // Maximum RTT
	Jitter     float64 // RTT jitter (ms) - mean deviation
	ErrorRate  float64 // Error rate (0-1)
	Throughput int64   // Throughput (bytes/sec) - smoothed

	// Statistics
	TotalPings  uint64 // Total ping count
	FailedPings uint64 // Failed ping count

	// Composite quality score (0-100)
	QualityScore float64

	// Last update time
	LastUpdated time.Time

	// For jitter calculation
	lastRTT float64
}

// NewLinkQualityMonitor creates a new LinkQualityMonitor.
func NewLinkQualityMonitor(p2pMgr *P2PManager, localID string) *LinkQualityMonitor {
	lqm := &LinkQualityMonitor{
		p2pMgr:        p2pMgr,
		localID:       localID,
		links:         make(map[string]*LinkMetrics),
		probeInterval: 5 * time.Second, // More frequent probing
		probeTimeout:  3 * time.Second,
		stopCh:        make(chan struct{}),
	}
	// Auto-start the monitor
	lqm.Start()
	return lqm
}

// Start starts the link quality monitor.
func (lqm *LinkQualityMonitor) Start() {
	go lqm.probeRTT()
	util.LogModuleInfo("P2P", "LinkQualityMonitor started")
}

// Stop stops the link quality monitor.
func (lqm *LinkQualityMonitor) Stop() {
	close(lqm.stopCh)
	util.LogModuleInfo("P2P", "LinkQualityMonitor stopped")
}

// probeRTT periodically probes RTT to all connected peers.
func (lqm *LinkQualityMonitor) probeRTT() {
	ticker := time.NewTicker(lqm.probeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// Probe each host's connections
			for _, h := range lqm.p2pMgr.GetAllHosts() {
				for _, conn := range h.Network().Conns() {
					connID := conn.ID()
					peerID := conn.RemotePeer()
					go func(cid string, pid peer.ID, host host.Host) {
						result := <-ping.Ping(lqm.p2pMgr.ctx, host, pid)
						if result.Error != nil {
							lqm.recordPingFailure(cid)
						} else {
							lqm.recordRTT(cid, result.RTT)
						}
					}(connID, peerID, h)
				}
			}
		case <-lqm.stopCh:
			return
		}
	}
}

// RecordRTT records RTT for a connection.
func (lqm *LinkQualityMonitor) RecordRTT(connID string, rtt time.Duration) {
	lqm.mu.Lock()
	defer lqm.mu.Unlock()

	metrics := lqm.links[connID]
	if metrics == nil {
		metrics = &LinkMetrics{}
		lqm.links[connID] = metrics
	}

	metrics.updateRTT(float64(rtt.Milliseconds()))
}

// GetLinkQuality gets link quality for a specific connection.
func (lqm *LinkQualityMonitor) GetLinkQuality(connID string) *LinkMetrics {
	lqm.mu.RLock()
	defer lqm.mu.RUnlock()

	return lqm.links[connID]
}

// GetAllLinkQualities gets all link qualities.
func (lqm *LinkQualityMonitor) GetAllLinkQualities() map[string]*LinkMetrics {
	lqm.mu.RLock()
	defer lqm.mu.RUnlock()

	result := make(map[string]*LinkMetrics)
	for connID, metrics := range lqm.links {
		result[connID] = metrics
	}
	return result
}

// recordPingFailure records a ping failure.
func (lqm *LinkQualityMonitor) recordPingFailure(connID string) {
	lqm.mu.Lock()
	defer lqm.mu.Unlock()

	metrics := lqm.links[connID]
	if metrics == nil {
		metrics = &LinkMetrics{}
		lqm.links[connID] = metrics
	}

	metrics.TotalPings++
	metrics.FailedPings++
	metrics.LastUpdated = time.Now()
	metrics.ErrorRate = float64(metrics.FailedPings) / float64(metrics.TotalPings)
	metrics.QualityScore = calculateQualityScore(metrics)

	util.LogModuleDebug("P2P", "Ping failed on connection %s", connID)
}

// recordRTT records a successful RTT measurement.
func (lqm *LinkQualityMonitor) recordRTT(connID string, rtt time.Duration) {
	lqm.mu.Lock()
	defer lqm.mu.Unlock()

	metrics := lqm.links[connID]
	if metrics == nil {
		metrics = &LinkMetrics{}
		lqm.links[connID] = metrics
	}

	metrics.updateRTT(float64(rtt.Milliseconds()))
	util.LogModuleDebug("P2P", "RTT on connection %s: %v", connID, rtt)
}

// updateRTT updates RTT statistics.
func (m *LinkMetrics) updateRTT(rttMs float64) {
	m.TotalPings++
	m.LastUpdated = time.Now()

	if m.RTTMin == 0 || rttMs < m.RTTMin {
		m.RTTMin = rttMs
	}
	if rttMs > m.RTTMax {
		m.RTTMax = rttMs
	}

	// Calculate jitter (mean absolute deviation)
	if m.lastRTT > 0 {
		deviation := math.Abs(rttMs - m.lastRTT)
		if m.Jitter == 0 {
			m.Jitter = deviation
		} else {
			m.Jitter = m.Jitter*0.8 + deviation*0.2 // Exponential moving average
		}
	}
	m.lastRTT = rttMs

	// Exponential moving average for RTT
	if m.RTT == 0 {
		m.RTT = rttMs
	} else {
		m.RTT = m.RTT*0.7 + rttMs*0.3
	}

	// Update quality score
	m.QualityScore = calculateQualityScore(m)
}

// calculateQualityScore calculates a composite quality score (0-100).
func calculateQualityScore(m *LinkMetrics) float64 {
	// RTT score: lower is better (baseline: 100ms = 0, 10ms = 100)
	rttScore := math.Max(0, 100-(m.RTT-10))

	// Jitter score: lower is better (0ms = 100, 50ms = 0)
	jitterScore := math.Max(0, 100-m.Jitter*2)

	// Error rate score: lower is better
	errorScore := (1 - m.ErrorRate) * 100

	// Throughput score: higher is better (log scale)
	throughputScore := math.Min(100, math.Log10(float64(m.Throughput)/1000+1)*20)

	// Weighted average: RTT and error rate are most important
	score := rttScore*0.3 + jitterScore*0.2 + errorScore*0.3 + throughputScore*0.2

	return math.Max(0, math.Min(100, score))
}
