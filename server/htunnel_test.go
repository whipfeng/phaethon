package server

import (
	"sync"
	"testing"
	"time"
)

// TestPendingOp_leaderOrWait_firstCaller verifies the first caller becomes leader.
func TestPendingOp_leaderOrWait_firstCaller(t *testing.T) {
	ch := &htChannel{
		id:     1,
		closed: make(chan struct{}),
	}

	ch.mu.Lock()
	isLeader := ch.leaderOrWait(&ch.readPend)
	// leaderOrWait releases the lock on the leader path
	if !isLeader {
		t.Error("first caller should be leader")
	}
}

// TestPendingOp_leaderOrWait_waiterBlocks verifies that when an op is active,
// subsequent callers block until broadcast and return false.
func TestPendingOp_leaderOrWait_waiterBlocks(t *testing.T) {
	ch := &htChannel{
		id:     1,
		closed: make(chan struct{}),
	}

	// Pre-set the op as active so the waiter will block
	ch.mu.Lock()
	ch.readPend.active = true
	ch.readPend.done = make(chan struct{})
	ch.mu.Unlock()

	done := make(chan bool, 1)
	go func() {
		ch.mu.Lock()
		isLeader := ch.leaderOrWait(&ch.readPend)
		ch.mu.Unlock()
		done <- isLeader
	}()

	// Waiter should still be blocked
	select {
	case <-done:
		t.Fatal("waiter should block until broadcast")
	case <-time.After(100 * time.Millisecond):
		// expected
	}

	// Broadcast wakes the waiter
	ch.mu.Lock()
	ch.broadcast(&ch.readPend)
	ch.mu.Unlock()

	select {
	case isLeader := <-done:
		if isLeader {
			t.Error("waiter should return false")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter did not wake after broadcast")
	}
}

// TestPendingOp_leaderOrWait_multipleRounds verifies that a new round
// can start after the previous one is broadcast.
func TestPendingOp_leaderOrWait_multipleRounds(t *testing.T) {
	ch := &htChannel{
		id:     1,
		closed: make(chan struct{}),
	}

	// Round 1
	ch.mu.Lock()
	isLeader := ch.leaderOrWait(&ch.connPend)
	// leaderOrWait unlocked on leader path
	if !isLeader {
		t.Error("first should be leader")
	}
	ch.mu.Lock()
	ch.broadcast(&ch.connPend)
	ch.mu.Unlock()

	// Round 2 (same op, after broadcast)
	ch.mu.Lock()
	isLeader = ch.leaderOrWait(&ch.connPend)
	// leaderOrWait unlocked on leader path
	if !isLeader {
		t.Error("second should also be leader after broadcast")
	}
	ch.mu.Lock()
	ch.broadcast(&ch.connPend)
	ch.mu.Unlock()
}

// TestPendingOp_leaderOrWait_multipleWaiters verifies broadcast wakes all waiters.
func TestPendingOp_leaderOrWait_multipleWaiters(t *testing.T) {
	ch := &htChannel{
		id:     1,
		closed: make(chan struct{}),
	}

	// Pre-set the op as active
	ch.mu.Lock()
	ch.readPend.active = true
	ch.readPend.done = make(chan struct{})
	ch.mu.Unlock()

	var wg sync.WaitGroup
	results := make(chan bool, 3)

	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch.mu.Lock()
			isLeader := ch.leaderOrWait(&ch.readPend)
			ch.mu.Unlock()
			if isLeader {
				t.Error("waiter became leader unexpectedly")
			}
			results <- isLeader
		}()
	}

	// Give waiters time to block
	time.Sleep(100 * time.Millisecond)

	ch.mu.Lock()
	ch.broadcast(&ch.readPend)
	ch.mu.Unlock()

	wg.Wait()
	close(results)

	waiterCount := 0
	for r := range results {
		if !r {
			waiterCount++
		}
	}
	if waiterCount != 3 {
		t.Errorf("expected 3 waiters, got %d", waiterCount)
	}
}

// TestHTChannel_resetReqTimeout verifies the timeout timer is set.
func TestHTChannel_resetReqTimeout(t *testing.T) {
	s := &HTunnelServer{}
	ch := &htChannel{
		id:     1,
		closed: make(chan struct{}),
	}

	ch.resetReqTimeout(s, 1)
	if ch.reqTimeout == nil {
		t.Fatal("reqTimeout should be set")
	}

	// Reset should replace the timer
	oldTimer := ch.reqTimeout
	ch.resetReqTimeout(s, 1)
	if ch.reqTimeout == oldTimer {
		t.Error("resetReqTimeout should create a new timer")
	}

	// Clean up
	ch.reqTimeout.Stop()
}

// TestHTChannel_closeChannel_idempotent verifies closeChannel is safe to call multiple times.
func TestHTChannel_closeChannel_idempotent(t *testing.T) {
	s := &HTunnelServer{}
	ch := &htChannel{
		id:     1,
		closed: make(chan struct{}),
	}
	s.channels.Store(int64(1), ch)

	// First close
	s.closeChannel(1)

	select {
	case <-ch.closed:
		// ok
	default:
		t.Error("channel should be closed")
	}

	// Second close should not panic
	s.closeChannel(1)
}
