package p2p

import (
	"math"
	"phaethon/util"
	"sync"
	"time"
)

// pendingFrame is a control frame waiting for acknowledgment.
type pendingFrame struct {
	frameType byte
	seq       uint32
	payload   []byte
	sentTime  time.Time
	retries   int
}

// frameRecord tracks a completed frame (acked or lost) for time-window loss calculation.
type frameRecord struct {
	timestamp time.Time
	lost      bool // true if lost, false if acked
}

// sendState tracks outgoing control frames for reliable delivery.
type sendState struct {
	mu           sync.Mutex
	nextSeq      uint32                   // next sequence number to assign
	pending      map[uint32]*pendingFrame // frames sent but not yet acked
	lastAck      uint32                   // last ack received from peer
	rttSampler   *rttSampler              // RTT estimation
	frameHistory []frameRecord            // history of completed frames (last 60s)
	totalSent    uint64                   // total control frames sent (cumulative, for backward compat)
	totalAcked   uint64                   // total control frames acked (cumulative)
	totalLost    uint64                   // total control frames lost (cumulative)
}

func newSendState() *sendState {
	return &sendState{
		nextSeq:    1, // start from 1, 0 means "no ack"
		pending:    make(map[uint32]*pendingFrame),
		rttSampler: newRTTSampler(),
	}
}

// allocSeq assigns the next sequence number.
func (s *sendState) allocSeq() uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	seq := s.nextSeq
	s.nextSeq++
	return seq
}

// getLastAck returns the last ack received from the peer.
func (s *sendState) getLastAck() uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastAck
}

// markSent records a frame as sent, waiting for ack.
func (s *sendState) markSent(seq uint32, frameType byte, payload []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[seq] = &pendingFrame{
		frameType: frameType,
		seq:       seq,
		payload:   payload,
		sentTime:  time.Now(),
		retries:   0,
	}
	s.totalSent++
}

// processAck handles an incoming ack, removing acknowledged frames and updating RTT.
func (s *sendState) processAck(ack uint32) {
	if ack == 0 {
		return // 0 means "no ack"
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if ack > s.lastAck {
		s.lastAck = ack
	}

	// Remove all frames up to and including the ack
	for seq := range s.pending {
		if seq <= ack {
			// Update RTT sampler
			if pf, ok := s.pending[seq]; ok {
				rtt := time.Since(pf.sentTime)
				isRetransmission := pf.retries > 0
				s.rttSampler.add(rtt, isRetransmission)
				s.totalAcked++
				// Record in history for time-window loss calculation
				s.frameHistory = append(s.frameHistory, frameRecord{
					timestamp: time.Now(),
					lost:      false,
				})
			}
			delete(s.pending, seq)
		}
	}
}

// getRetransmissions returns frames that need retransmission (timed out).
// getRetransmissions returns frames that have timed out and need retransmission.
// Uses exponential backoff: timeout = baseRTO * 2^retries
func (s *sendState) getRetransmissions(baseRTO time.Duration) []*pendingFrame {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	var result []*pendingFrame
	for _, pf := range s.pending {
		// Exponential backoff: RTO, 2*RTO, 4*RTO, 8*RTO, 16*RTO...
		timeout := baseRTO * time.Duration(1<<pf.retries)
		// Cap at 60 seconds like TCP
		if timeout > 60*time.Second {
			timeout = 60 * time.Second
		}
		if now.Sub(pf.sentTime) > timeout {
			result = append(result, pf)
		}
	}
	return result
}

// markRetransmitted updates the sent time for a retransmitted frame.
func (s *sendState) markRetransmitted(seq uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pf, ok := s.pending[seq]; ok {
		pf.sentTime = time.Now()
		pf.retries++
	}
}

// removeFrame removes a frame from pending (gave up after max retries).
func (s *sendState) removeFrame(seq uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pending[seq]; ok {
		s.totalLost++
		// Record in history for time-window loss calculation
		s.frameHistory = append(s.frameHistory, frameRecord{
			timestamp: time.Now(),
			lost:      true,
		})
	}
	delete(s.pending, seq)
}

// recvState tracks incoming control frames for deduplication.
type recvState struct {
	mu      sync.Mutex
	lastSeq uint32            // highest seq received (for generating ack)
	seen    map[uint32]time.Time // recent seqs for dedup
}

func newRecvState() *recvState {
	return &recvState{
		seen: make(map[uint32]time.Time),
	}
}

// checkAndRecord checks if a frame is a duplicate and records it.
// Returns true if the frame is new, false if duplicate.
func (s *recvState) checkAndRecord(seq uint32) bool {
	if seq == 0 {
		return true // seq 0 means "no seq" (data frames)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Check if duplicate
	if _, ok := s.seen[seq]; ok {
		return false // duplicate
	}

	// Record as seen
	s.seen[seq] = time.Now()
	if seq > s.lastSeq {
		s.lastSeq = seq
	}

	// Clean up old entries (keep last 1000 or 60s)
	s.cleanup()

	return true
}

// getLastSeq returns the last received seq (for generating ack).
func (s *recvState) getLastSeq() uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSeq
}

// cleanup removes old entries from the seen map.
func (s *recvState) cleanup() {
	// Keep at most 1000 entries
	if len(s.seen) <= 1000 {
		return
	}

	// Remove entries older than 60s
	cutoff := time.Now().Add(-60 * time.Second)
	for seq, t := range s.seen {
		if t.Before(cutoff) {
			delete(s.seen, seq)
		}
	}
}

// rttSampler tracks RTT samples for estimation.
type rttRecord struct {
	timestamp        time.Time
	rtt              time.Duration
	isRetransmission bool
}

type rttSampler struct {
	mu      sync.Mutex
	samples []rttRecord
	window  time.Duration // 5 minutes
	srtt    time.Duration // smoothed RTT (for RTO calculation)
	rttvar  time.Duration // RTT variance (for RTO calculation)
}

func newRTTSampler() *rttSampler {
	return &rttSampler{
		samples: make([]rttRecord, 0, 100),
		window:  5 * time.Minute,
	}
}

// add adds an RTT sample with retransmission flag.
func (s *rttSampler) add(rtt time.Duration, isRetransmission bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Add new sample
	s.samples = append(s.samples, rttRecord{
		timestamp:        time.Now(),
		rtt:              rtt,
		isRetransmission: isRetransmission,
	})

	// Cleanup old samples
	s.cleanup()

	// Update smoothed RTT using weighted algorithm
	// Accept all samples, but retransmission samples have lower weight
	// This avoids the death spiral where high-latency paths never collect samples
	if s.srtt == 0 {
		// First sample
		s.srtt = rtt
		s.rttvar = rtt / 2
	} else if isRetransmission {
		// Retransmission sample: lower weight (1/16 instead of 1/8)
		// These samples are less reliable but still useful for high-latency paths
		diff := s.srtt - rtt
		if diff < 0 {
			diff = -diff
		}
		s.rttvar = (15*s.rttvar + diff) / 16
		s.srtt = (15*s.srtt + rtt) / 16
	} else {
		// First transmission sample: normal weight (1/8)
		// SRTT = (1 - alpha) * SRTT + alpha * R, where alpha = 1/8
		diff := s.srtt - rtt
		if diff < 0 {
			diff = -diff
		}
		s.rttvar = (3*s.rttvar + diff) / 4
		s.srtt = (7*s.srtt + rtt) / 8
	}
}

// cleanup removes samples older than the window (5 minutes).
func (s *rttSampler) cleanup() {
	cutoff := time.Now().Add(-s.window)
	var recent []rttRecord
	for _, r := range s.samples {
		if r.timestamp.After(cutoff) {
			recent = append(recent, r)
		}
	}
	s.samples = recent
}

// getSRTT returns the average RTT from all samples in the window.
func (s *rttSampler) getSRTT() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cleanup()

	// Calculate average from all samples (including retransmissions)
	var sum time.Duration
	var count int
	for _, r := range s.samples {
		sum += r.rtt
		count++
	}

	if count == 0 {
		return 0
	}
	return sum / time.Duration(count)
}

// getSampleCount returns the number of first transmission samples in the window.
func (s *rttSampler) getSampleCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cleanup()

	// Count all samples (including retransmissions)
	return len(s.samples)
}

// getJitter returns the standard deviation of RTT samples (first transmission only).
func (s *rttSampler) getJitter() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cleanup()

	// Collect all samples (including retransmissions)
	var samples []time.Duration
	for _, r := range s.samples {
		samples = append(samples, r.rtt)
	}

	if len(samples) < 2 {
		return 0
	}

	// Calculate mean
	var sum time.Duration
	for _, rtt := range samples {
		sum += rtt
	}
	mean := sum / time.Duration(len(samples))

	// Calculate variance
	var variance float64
	for _, rtt := range samples {
		diff := float64(rtt - mean)
		variance += diff * diff
	}
	variance /= float64(len(samples))

	// Return standard deviation
	return time.Duration(math.Sqrt(variance))
}

// getRTO returns the retransmission timeout based on RTT.
// RTO = SRTT + max(G, K*RTTVAR), where G = clock granularity, K = 4
func (s *rttSampler) getRTO() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.srtt == 0 {
		return time.Second // default RTO before any samples
	}

	// Clock granularity (assume 10ms)
	g := 10 * time.Millisecond
	k := 4

	rto := s.srtt + time.Duration(k)*s.rttvar
	if rto < g {
		rto = g
	}
	return rto
}

// cleanupOldRecords removes frame history older than 5 minutes.
func (s *sendState) cleanupOldRecords() {
	cutoff := time.Now().Add(-5 * time.Minute)
	var recent []frameRecord
	for _, r := range s.frameHistory {
		if r.timestamp.After(cutoff) {
			recent = append(recent, r)
		}
	}
	s.frameHistory = recent
}

// getLossRate returns the estimated loss rate based on frames lost vs sent in the last 60 seconds.
func (s *sendState) getLossRate() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	
	s.cleanupOldRecords()
	
	var sent, lost int
	for _, r := range s.frameHistory {
		sent++
		if r.lost {
			lost++
		}
	}
	
	if sent == 0 {
		return 0
	}
	return float64(lost) / float64(sent)
}

// getStats returns send state statistics.
// Loss rate is calculated from the last 5 minutes of frame history, considering pending frames.
func (s *sendState) getStats() (srtt, rto, jitter time.Duration, lossRate float64, sampleCount int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	
	s.cleanupOldRecords()
	
	srtt = s.rttSampler.getSRTT()
	rto = s.rttSampler.getRTO()
	jitter = s.rttSampler.getJitter()
	sampleCount = s.rttSampler.getSampleCount()
	
	// Calculate loss rate from last 5 minutes
	var sent, lost, pendingOld int
	now := time.Now()
	
	// Count completed frames
	for _, r := range s.frameHistory {
		sent++
		if r.lost {
			lost++
		}
	}
	
	// Count "possibly lost" pending frames (pending > 2 * RTO)
	for _, pf := range s.pending {
		if now.Sub(pf.sentTime) > 2*rto {
			pendingOld++
		}
	}
	
	// Loss rate = (lost + pendingOld) / (sent + pendingOld)
	totalWithPending := sent + pendingOld
	if totalWithPending > 0 {
		lossRate = float64(lost+pendingOld) / float64(totalWithPending)
	}
	
	// Log stats when queried
	util.LogInfo("[P2P-STATS] window=%ds sent=%d lost=%d pendingOld=%d lossRate=%.4f jitter=%v samples=%d (cumulative: totalSent=%d totalAcked=%d totalLost=%d)", 
		300, sent, lost, pendingOld, lossRate, jitter, sampleCount, s.totalSent, s.totalAcked, s.totalLost)
	return
}
