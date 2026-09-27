package mesh

import (
	"sync"
	"time"
)

// PeerQuality tracks the link quality to a direct peer.
type PeerQuality struct {
	mu                sync.RWMutex
	rttWindow         []time.Duration // sliding window of RTT samples
	rttIdx            int             // current index in circular buffer
	rttCount          int             // number of samples (up to window size)
	sent              int             // probes sent
	received          int             // probe replies received
	consecutiveFailed int             // consecutive probe failures
	lastProbeSeq      uint32          // last probe sequence number sent
	lastReplySeq      uint32          // last probe reply sequence number received
	lastUpdate        time.Time
}

// NewPeerQuality creates a new PeerQuality tracker.
func NewPeerQuality() *PeerQuality {
	return &PeerQuality{
		rttWindow: make([]time.Duration, 10), // default window size 10
	}
}

// RecordRTT adds an RTT sample to the sliding window.
func (q *PeerQuality) RecordRTT(rtt time.Duration, seq uint32) {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.rttWindow[q.rttIdx] = rtt
	q.rttIdx = (q.rttIdx + 1) % len(q.rttWindow)
	if q.rttCount < len(q.rttWindow) {
		q.rttCount++
	}
	q.received++
	q.lastReplySeq = seq
	q.consecutiveFailed = 0 // reset on success
	q.lastUpdate = time.Now()
}

// RecordSent increments the sent counter and records the probe sequence number.
func (q *PeerQuality) RecordSent(seq uint32) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.sent++
	// Check if we received the previous probe
	if q.lastProbeSeq > 0 && q.lastReplySeq < q.lastProbeSeq {
		// Previous probe was not replied
		q.consecutiveFailed++
	}
	q.lastProbeSeq = seq
}

// RecordFailure increments the consecutive failure counter.
func (q *PeerQuality) RecordFailure() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.consecutiveFailed++
}

// ConsecutiveFailures returns the number of consecutive probe failures.
func (q *PeerQuality) ConsecutiveFailures() int {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return q.consecutiveFailed
}

// AverageRTT returns the average RTT over the sliding window.
// Returns 0 if no samples.
func (q *PeerQuality) AverageRTT() time.Duration {
	q.mu.RLock()
	defer q.mu.RUnlock()

	if q.rttCount == 0 {
		return 0
	}

	var sum time.Duration
	for i := 0; i < q.rttCount; i++ {
		sum += q.rttWindow[i]
	}
	return sum / time.Duration(q.rttCount)
}

// PacketLoss returns the packet loss ratio (0.0 - 1.0).
// Returns 0 if no probes sent.
func (q *PeerQuality) PacketLoss() float64 {
	q.mu.RLock()
	defer q.mu.RUnlock()

	if q.sent == 0 {
		return 0
	}
	lost := q.sent - q.received
	if lost < 0 {
		lost = 0
	}
	return float64(lost) / float64(q.sent)
}

// LastUpdate returns the time of the last quality update.
func (q *PeerQuality) LastUpdate() time.Time {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return q.lastUpdate
}

// Stats returns a snapshot of the quality metrics.
func (q *PeerQuality) Stats() (avgRTT time.Duration, loss float64) {
	return q.AverageRTT(), q.PacketLoss()
}

// Counters returns the sent and received counters.
func (q *PeerQuality) Counters() (sent, received int) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return q.sent, q.received
}

// Reset resets all counters and RTT samples.
func (q *PeerQuality) Reset() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.sent = 0
	q.received = 0
	q.consecutiveFailed = 0
	q.rttIdx = 0
	q.rttCount = 0
	q.lastUpdate = time.Time{}
}

// PeerQualityTracker manages quality tracking for all peers.
type PeerQualityTracker struct {
	mu     sync.RWMutex
	peers  map[string]*PeerQuality // nodeID -> quality
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
