package p2p

import (
	"math"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/protocol/ping"

	"phaethon/util"
)

// LinkQualityMonitor monitors link quality for each proxy link.
type LinkQualityMonitor struct {
	p2pMgr  *P2PManager
	localID string

	mu sync.RWMutex

	// Link quality by (proxyName, peerID)
	links map[string]map[peer.ID]*LinkMetrics // proxyName -> peerID -> metrics

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
	ErrorRate  float64 // Error rate (0-1)
	Throughput int64   // Throughput (bytes/sec)

	// Statistics
	TotalPings  uint64 // Total ping count
	FailedPings uint64 // Failed ping count

	// Composite quality score (0-100)
	QualityScore float64

	// Last update time
	LastUpdated time.Time
}

// NewLinkQualityMonitor creates a new LinkQualityMonitor.
func NewLinkQualityMonitor(p2pMgr *P2PManager, localID string) *LinkQualityMonitor {
	return &LinkQualityMonitor{
		p2pMgr:        p2pMgr,
		localID:       localID,
		links:         make(map[string]map[peer.ID]*LinkMetrics),
		probeInterval: 10 * time.Second,
		probeTimeout:  5 * time.Second,
		stopCh:        make(chan struct{}),
	}
}

// Start starts the link quality monitor.
func (lqm *LinkQualityMonitor) Start() {
	go lqm.probeRTT()
	util.LogInfo("P2P", "LinkQualityMonitor started")
}

// Stop stops the link quality monitor.
func (lqm *LinkQualityMonitor) Stop() {
	close(lqm.stopCh)
	util.LogInfo("P2P", "LinkQualityMonitor stopped")
}

// probeRTT periodically probes RTT to all connected peers.
func (lqm *LinkQualityMonitor) probeRTT() {
	ticker := time.NewTicker(lqm.probeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// Probe each host's connections
			for proxyName, h := range lqm.p2pMgr.GetAllHosts() {
				for _, conn := range h.Network().Conns() {
					peerID := conn.RemotePeer()
					go func(pn string, pid peer.ID) {
						result := <-ping.Ping(lqm.p2pMgr.ctx, lqm.p2pMgr.hosts[pn], pid)
						if result.Error != nil {
							lqm.recordPingFailure(pn, pid)
						} else {
							lqm.recordRTT(pn, pid, result.RTT)
						}
					}(proxyName, peerID)
				}
			}
		case <-lqm.stopCh:
			return
		}
	}
}

// RecordRTT records RTT for a link.
func (lqm *LinkQualityMonitor) RecordRTT(proxyName string, peerID peer.ID, rtt time.Duration) {
	lqm.mu.Lock()
	defer lqm.mu.Unlock()

	if lqm.links[proxyName] == nil {
		lqm.links[proxyName] = make(map[peer.ID]*LinkMetrics)
	}

	metrics := lqm.links[proxyName][peerID]
	if metrics == nil {
		metrics = &LinkMetrics{}
		lqm.links[proxyName][peerID] = metrics
	}

	metrics.updateRTT(float64(rtt.Milliseconds()))
}

// GetLinkQuality gets link quality for a specific proxy and peer.
func (lqm *LinkQualityMonitor) GetLinkQuality(proxyName string, peerID peer.ID) *LinkMetrics {
	lqm.mu.RLock()
	defer lqm.mu.RUnlock()

	if lqm.links[proxyName] == nil {
		return nil
	}
	return lqm.links[proxyName][peerID]
}

// GetAllLinkQualities gets all link qualities for a proxy.
func (lqm *LinkQualityMonitor) GetAllLinkQualities(proxyName string) map[peer.ID]*LinkMetrics {
	lqm.mu.RLock()
	defer lqm.mu.RUnlock()

	result := make(map[peer.ID]*LinkMetrics)
	if links, ok := lqm.links[proxyName]; ok {
		for pid, metrics := range links {
			result[pid] = metrics
		}
	}
	return result
}

// recordPingFailure records a ping failure.
func (lqm *LinkQualityMonitor) recordPingFailure(proxyName string, peerID peer.ID) {
	lqm.mu.Lock()
	defer lqm.mu.Unlock()

	if lqm.links[proxyName] == nil {
		lqm.links[proxyName] = make(map[peer.ID]*LinkMetrics)
	}

	metrics := lqm.links[proxyName][peerID]
	if metrics == nil {
		metrics = &LinkMetrics{}
		lqm.links[proxyName][peerID] = metrics
	}

	metrics.TotalPings++
	metrics.FailedPings++
	metrics.LastUpdated = time.Now()
	metrics.ErrorRate = float64(metrics.FailedPings) / float64(metrics.TotalPings)
	metrics.QualityScore = calculateQualityScore(metrics)

	util.LogDebug("P2P", "Ping failed to peer %s via proxy %s", peerID, proxyName)
}

// recordRTT records a successful RTT measurement.
func (lqm *LinkQualityMonitor) recordRTT(proxyName string, peerID peer.ID, rtt time.Duration) {
	lqm.mu.Lock()
	defer lqm.mu.Unlock()

	if lqm.links[proxyName] == nil {
		lqm.links[proxyName] = make(map[peer.ID]*LinkMetrics)
	}

	metrics := lqm.links[proxyName][peerID]
	if metrics == nil {
		metrics = &LinkMetrics{}
		lqm.links[proxyName][peerID] = metrics
	}

	metrics.updateRTT(float64(rtt.Milliseconds()))
	util.LogDebug("P2P", "RTT to peer %s via proxy %s: %v", peerID, proxyName, rtt)
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

	// Exponential moving average
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

	// Error rate score: lower is better
	errorScore := (1 - m.ErrorRate) * 100

	// Throughput score: higher is better (log scale)
	throughputScore := math.Min(100, math.Log10(float64(m.Throughput)/1000+1)*20)

	// Weighted average
	score := rttScore*0.4 + errorScore*0.4 + throughputScore*0.2

	return math.Max(0, math.Min(100, score))
}
