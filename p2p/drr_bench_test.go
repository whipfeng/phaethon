package p2p

import (
	"sync/atomic"
	"testing"
	"time"
)

// BenchmarkDRRNextBatch measures the CPU cost of one DRR scheduling round.
func BenchmarkDRRNextBatch(b *testing.B) {
	// Create a DRR scheduler with 256 queues, each with some data.
	var queues [256]chan writeReq
	for i := range queues {
		queues[i] = make(chan writeReq, 512)
	}
	var budget atomic.Int64
	sched := newDRRScheduler(queues, &budget)

	// Pre-fill all queues with one frame each.
	for i := 0; i < 256; i++ {
		queues[i] <- writeReq{
			data:        make([]byte, 1500), // typical MTU
			enqueueTime: time.Now(),
		}
	}

	stopCh := make(chan struct{})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		batch := sched.NextBatch(64, stopCh)
		// Refill queues to keep the scheduler busy.
		for range batch {
			queues[i%256] <- writeReq{
				data:        make([]byte, 1500),
				enqueueTime: time.Now(),
			}
		}
	}
}
