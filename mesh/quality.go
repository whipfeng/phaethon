package mesh

import (
	"fmt"
	"math"
	"sync"
	"time"
)

// PeerQuality tracks the link quality to a direct peer using ACK-based passive measurement.
type PeerQuality struct {
	mu sync.RWMutex

	// ACK-based passive measurement (v7)
	ackRTT        time.Duration // smoothed RTT from ACK-based measurement
	ackLossRate   float64       // loss rate from ACK-based measurement
	ackJitter     time.Duration // jitter (RTT standard deviation)
	ackSampleCount int          // number of samples in the window
	ackUpdated    time.Time     // last time ACK stats were updated
}

// NewPeerQuality creates a new PeerQuality tracker.
func NewPeerQuality() *PeerQuality {
	return &PeerQuality{}
}

// Stats returns a snapshot of the quality metrics.
// Returns ACK-based passive measurement (srtt, lossRate, jitter).
func (q *PeerQuality) Stats() (avgRTT time.Duration, loss, jitter float64) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return q.ackRTT, q.ackLossRate, float64(q.ackJitter.Milliseconds())
}

// StatsWithConfidence returns quality metrics with confidence score.
func (q *PeerQuality) StatsWithConfidence() (avgRTT time.Duration, loss, jitter float64, confidence float64) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	
	avgRTT = q.ackRTT
	loss = q.ackLossRate
	jitter = float64(q.ackJitter.Milliseconds())
	confidence = q.calculateConfidence()
	
	return
}

// calculateConfidence calculates confidence based on sample count and time freshness.
func (q *PeerQuality) calculateConfidence() float64 {
	// Sample factor: need at least 10 samples for statistical significance
	sampleFactor := math.Min(1.0, float64(q.ackSampleCount)/10.0)
	
	// Time factor: more recent data is more trustworthy (1 minute decay)
	age := time.Since(q.ackUpdated)
	timeFactor := math.Exp(-age.Minutes())
	
	return sampleFactor * timeFactor
}

// UpdateACKStats updates the ACK-based passive quality measurements.
// Called by mesh layer when pulling stats from P2P sendState.
func (q *PeerQuality) UpdateACKStats(srtt time.Duration, lossRate float64, jitter time.Duration, sampleCount int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.ackRTT = srtt
	q.ackLossRate = lossRate
	q.ackJitter = jitter
	q.ackSampleCount = sampleCount
	q.ackUpdated = time.Now()
}

// LastACKUpdate returns the time of the last ACK-based stats update.
func (q *PeerQuality) LastACKUpdate() time.Time {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return q.ackUpdated
}

// IsStale returns true if the stats are older than the given TTL.
func (q *PeerQuality) IsStale(ttl time.Duration) bool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return time.Since(q.ackUpdated) > ttl
}

// Reset resets all counters and RTT samples.
func (q *PeerQuality) Reset() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.ackRTT = 0
	q.ackLossRate = 0
	q.ackJitter = 0
	q.ackSampleCount = 0
	q.ackUpdated = time.Time{}
}

// PeerQualityTracker manages quality tracking for all peers.
// Supports per-link tracking: each (nodeID, localSeq) pair has its own quality metrics.
type PeerQualityTracker struct {
	mu    sync.RWMutex
	peers map[string]*PeerQuality // key: "nodeID:localSeq" -> quality
}

// NewPeerQualityTracker creates a new tracker.
func NewPeerQualityTracker() *PeerQualityTracker {
	return &PeerQualityTracker{
		peers: make(map[string]*PeerQuality),
	}
}

// makeKey creates a composite key for per-link tracking.
func makeKey(nodeID string, localSeq uint16) string {
	return fmt.Sprintf("%s:%d", nodeID, localSeq)
}

// Get returns the quality tracker for a specific link, creating if needed.
func (t *PeerQualityTracker) Get(nodeID string, localSeq uint16) *PeerQuality {
	t.mu.Lock()
	defer t.mu.Unlock()

	key := makeKey(nodeID, localSeq)
	if q, ok := t.peers[key]; ok {
		return q
	}
	q := NewPeerQuality()
	t.peers[key] = q
	return q
}

// GetByNode returns the quality tracker for a node (first link found).
// Deprecated: Use Get(nodeID, localSeq) for per-link tracking.
func (t *PeerQualityTracker) GetByNode(nodeID string) *PeerQuality {
	t.mu.RLock()
	defer t.mu.RUnlock()

	// Find first link for this node (for backward compatibility)
	prefix := nodeID + ":"
	for key, q := range t.peers {
		if len(key) > len(prefix) && key[:len(prefix)] == prefix {
			return q
		}
	}
	return nil
}

// Remove removes a specific link from tracking.
func (t *PeerQualityTracker) Remove(nodeID string, localSeq uint16) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.peers, makeKey(nodeID, localSeq))
}

// RemoveNode removes all links for a node from tracking.
func (t *PeerQualityTracker) RemoveNode(nodeID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	
	prefix := nodeID + ":"
	for key := range t.peers {
		if len(key) > len(prefix) && key[:len(prefix)] == prefix {
			delete(t.peers, key)
		}
	}
}

// GetAll returns a snapshot of all link qualities.
// Returns map[key]*PeerQuality where key is "nodeID:localSeq".
func (t *PeerQualityTracker) GetAll() map[string]*PeerQuality {
	t.mu.RLock()
	defer t.mu.RUnlock()

	result := make(map[string]*PeerQuality, len(t.peers))
	for k, v := range t.peers {
		result[k] = v
	}
	return result
}

// GetNodeLinks returns all links for a specific node.
func (t *PeerQualityTracker) GetNodeLinks(nodeID string) map[string]*PeerQuality {
	t.mu.RLock()
	defer t.mu.RUnlock()

	result := make(map[string]*PeerQuality)
	prefix := nodeID + ":"
	for key, q := range t.peers {
		if len(key) > len(prefix) && key[:len(prefix)] == prefix {
			localSeqStr := key[len(prefix):]
			result[localSeqStr] = q
		}
	}
	return result
}
