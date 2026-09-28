package mesh

import (
	"sync"
	"time"
)

// PeerQuality tracks the link quality to a direct peer using ACK-based passive measurement.
type PeerQuality struct {
	mu sync.RWMutex

	// ACK-based passive measurement (v7)
	ackRTT     time.Duration // smoothed RTT from ACK-based measurement
	ackLossRate float64      // loss rate from ACK-based measurement
	ackUpdated time.Time     // last time ACK stats were updated
}

// NewPeerQuality creates a new PeerQuality tracker.
func NewPeerQuality() *PeerQuality {
	return &PeerQuality{}
}

// Stats returns a snapshot of the quality metrics.
// Returns ACK-based passive measurement (srtt, lossRate).
func (q *PeerQuality) Stats() (avgRTT time.Duration, loss float64) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return q.ackRTT, q.ackLossRate
}

// UpdateACKStats updates the ACK-based passive quality measurements.
// Called by mesh layer when pulling stats from P2P sendState.
func (q *PeerQuality) UpdateACKStats(srtt time.Duration, lossRate float64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.ackRTT = srtt
	q.ackLossRate = lossRate
	q.ackUpdated = time.Now()
}

// LastACKUpdate returns the time of the last ACK-based stats update.
func (q *PeerQuality) LastACKUpdate() time.Time {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return q.ackUpdated
}

// Reset resets all counters and RTT samples.
func (q *PeerQuality) Reset() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.ackRTT = 0
	q.ackLossRate = 0
	q.ackUpdated = time.Time{}
}

// PeerQualityTracker manages quality tracking for all peers.
type PeerQualityTracker struct {
	mu    sync.RWMutex
	peers map[string]*PeerQuality // nodeID -> quality
}

// NewPeerQualityTracker creates a new tracker.
func NewPeerQualityTracker() *PeerQualityTracker {
	return &PeerQualityTracker{
		peers: make(map[string]*PeerQuality),
	}
}

// Get returns the quality tracker for a peer, creating if needed.
func (t *PeerQualityTracker) Get(nodeID string) *PeerQuality {
	t.mu.Lock()
	defer t.mu.Unlock()

	if q, ok := t.peers[nodeID]; ok {
		return q
	}
	q := NewPeerQuality()
	t.peers[nodeID] = q
	return q
}

// Remove removes a peer from tracking.
func (t *PeerQualityTracker) Remove(nodeID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.peers, nodeID)
}

// GetAll returns a snapshot of all peer qualities.
func (t *PeerQualityTracker) GetAll() map[string]*PeerQuality {
	t.mu.RLock()
	defer t.mu.RUnlock()

	result := make(map[string]*PeerQuality, len(t.peers))
	for k, v := range t.peers {
		result[k] = v
	}
	return result
}
