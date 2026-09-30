package mesh

import (
	"fmt"
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
