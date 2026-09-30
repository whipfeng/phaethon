package p2p

import (
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

// sendState tracks outgoing control frames for reliable delivery.
type sendState struct {
	mu         sync.Mutex
	nextSeq    uint32                   // next sequence number to assign
	pending    map[uint32]*pendingFrame // frames sent but not yet acked
	lastAck    uint32                   // last ack received from peer
	rttSampler *rttSampler              // RTT estimation
	totalSent  uint64                   // total control frames sent
	totalAcked uint64                   // total control frames acked
	totalLost  uint64                   // total control frames lost (gave up after max retries)
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
				s.rttSampler.add(rtt)
				s.totalAcked++
			}
			delete(s.pending, seq)
		}
	}
}

// getRetransmissions returns frames that need retransmission (timed out).
func (s *sendState) getRetransmissions(timeout time.Duration) []*pendingFrame {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	var result []*pendingFrame
	for _, pf := range s.pending {
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
type rttSampler struct {
	mu      sync.Mutex
	samples []time.Duration
	srtt    time.Duration // smoothed RTT
	rttvar  time.Duration // RTT variance
}

func newRTTSampler() *rttSampler {
	return &rttSampler{
		samples: make([]time.Duration, 0, 100),
	}
}

// add adds an RTT sample and updates the smoothed estimate.
func (s *rttSampler) add(rtt time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Keep last 100 samples
	if len(s.samples) >= 100 {
		s.samples = s.samples[1:]
	}
	s.samples = append(s.samples, rtt)

	// Update smoothed RTT using TCP-like algorithm (RFC 6298)
	if s.srtt == 0 {
		// First sample
		s.srtt = rtt
		s.rttvar = rtt / 2
	} else {
		// SRTT = (1 - alpha) * SRTT + alpha * R, where alpha = 1/8
		diff := s.srtt - rtt
		if diff < 0 {
			diff = -diff
		}
		s.rttvar = (3*s.rttvar + diff) / 4
		s.srtt = (7*s.srtt + rtt) / 8
	}
}

// getSRTT returns the smoothed RTT.
func (s *rttSampler) getSRTT() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.srtt
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

// getLossRate returns the estimated loss rate based on frames lost vs sent.
func (s *sendState) getLossRate() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.totalSent == 0 {
		return 0
	}
	return float64(s.totalLost) / float64(s.totalSent)
}

// getStats returns send state statistics.
func (s *sendState) getStats() (srtt, rto time.Duration, lossRate float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	srtt = s.rttSampler.getSRTT()
	rto = s.rttSampler.getRTO()
	if s.totalSent > 0 {
		lossRate = float64(s.totalLost) / float64(s.totalSent)
	}
	// Always log stats when queried
	util.LogInfo("[P2P-STATS] totalSent=%d totalAcked=%d totalLost=%d lossRate=%.4f", 
		s.totalSent, s.totalAcked, s.totalLost, lossRate)
	return
}
