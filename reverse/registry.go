package reverse

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"phaethon/util"
)

// ManagedConn wraps a net.Conn with a dedicated read goroutine
// to avoid concurrent Read() races between the heartbeat handler
// and Registry.Match handshake.
//
// Uses Unified Reverse Frame Protocol: all messages are framed with
// TYPE(1B) + LENGTH(2B) + PAYLOAD.
type ManagedConn struct {
	net.Conn
	stopCh       chan struct{}
	doneCh       chan struct{} // closed when readLoop exits
	once         sync.Once
	stopPing     chan struct{} // closed to stop heartbeat sender; managed by Match
	stopPingOnce sync.Once     // ensures stopPing is closed only once
	matched      bool          // true after reverseHandshake succeeds, prevents watcher cleanup
	matchedMu    sync.Mutex    // protects matched
	writeMu      sync.Mutex    // protects concurrent writes to Conn
	lastReadMu   sync.Mutex
	lastReadTime time.Time // last time readLoop successfully read a frame

	pendingMu  sync.Mutex
	pendingMsg byte
	hasPending bool
	senderWg   sync.WaitGroup // waits for heartbeat sender to exit
}

// NewManagedConn creates a new ManagedConn and starts the read loop.
func NewManagedConn(conn net.Conn) *ManagedConn {
	mc := &ManagedConn{
		Conn:   conn,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
	go mc.readLoop()
	return mc
}

func (mc *ManagedConn) readLoop() {
	defer close(mc.doneCh)

	for {
		select {
		case <-mc.stopCh:
			return
		default:
		}

		mc.Conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		frameType, _, err := ReadFrame(mc.Conn)
		if err != nil {
			return
		}

		mc.lastReadMu.Lock()
		mc.lastReadTime = time.Now()
		mc.lastReadMu.Unlock()

		if frameType == FrameHeartbeat {
			continue // heartbeat: drop immediately
		}

		// Save the first non-heartbeat message (PENG/PONG) for handshake.
		// Only store if nothing is pending yet to avoid overwriting.
		mc.pendingMu.Lock()
		if !mc.hasPending {
			mc.pendingMsg = frameType
			mc.hasPending = true
		}
		mc.pendingMu.Unlock()

		select {
		case <-mc.stopCh:
			return
		default:
		}
	}
}

// WriteMsg writes a control frame (type only, no payload) in a thread-safe manner.
func (mc *ManagedConn) WriteMsg(frameType byte) error {
	mc.writeMu.Lock()
	defer mc.writeMu.Unlock()
	return WriteFrame(mc.Conn, frameType, nil)
}

// Stop signals the read loop to stop without closing the underlying connection.
// After Stop, the underlying net.Conn.Read is safe for external callers.
// Stop blocks until the readLoop has fully exited.
func (mc *ManagedConn) Stop() {
	mc.once.Do(func() {
		close(mc.stopCh)
		mc.Conn.SetReadDeadline(time.Now()) // interrupt pending Read
	})
	<-mc.doneCh                          // wait for readLoop to fully exit
	mc.Conn.SetReadDeadline(time.Time{}) // clear deadline
}

// Registry manages reverse connections
type Registry struct {
	mu       sync.Mutex
	bottoms  map[string][]*ManagedConn      // address -> available bottom connections
	waiters  map[string][]chan *ManagedConn // address -> waiting channels
	cancelCh chan struct{}                  // closed when registry is refreshed; cancels pending waiters
}

var (
	globalRegistry *Registry
	registryMu     sync.Mutex
)

// Refresh creates a new global registry and immediately closes all unmatched
// connections in the old registry. Pending waiters are cancelled so they
// return immediately instead of waiting for a dead registry. Matched
// connections (already handed to a dialer) are left alone.
func Refresh() {
	registryMu.Lock()
	defer registryMu.Unlock()

	oldRegistry := globalRegistry
	globalRegistry = &Registry{
		bottoms:  make(map[string][]*ManagedConn),
		waiters:  make(map[string][]chan *ManagedConn),
		cancelCh: make(chan struct{}),
	}

	if oldRegistry != nil {
		oldRegistry.mu.Lock()
		// Close all unmatched connections from the old registry immediately.
		for _, bottoms := range oldRegistry.bottoms {
			for _, mc := range bottoms {
				mc.matchedMu.Lock()
				matched := mc.matched
				mc.matchedMu.Unlock()
				if !matched {
					mc.Conn.Close()
				}
			}
		}
		oldRegistry.mu.Unlock()
		close(oldRegistry.cancelCh)
	}
}

// GlobalRegistry returns the current global registry
func GlobalRegistry() *Registry {
	registryMu.Lock()
	defer registryMu.Unlock()
	return globalRegistry
}

// Register adds a bottom connection for the given reverse address
func (r *Registry) Register(address string, mc *ManagedConn) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Registry has been refreshed — reject new registrations to the old registry.
	select {
	case <-r.cancelCh:
		mc.Conn.Close()
		return
	default:
	}

	// Try to match a waiter first
	waiters := r.waiters[address]
	for len(waiters) > 0 {
		waiter := waiters[0]
		waiters = waiters[1:]
		r.waiters[address] = waiters

		select {
		case waiter <- mc:
			util.LogDebug("[REVERSE] hit: %s", address)
			return
		default:
			// waiter expired, try next
		}
	}

	// No waiter, queue the bottom connection
	r.bottoms[address] = append(r.bottoms[address], mc)
	util.LogDebug("[REVERSE] register: %s, pool=%d", address, len(r.bottoms[address]))
}

// Unregister removes a bottom connection.
// Returns true if the connection was found and removed.
func (r *Registry) Unregister(address string, mc *ManagedConn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	bottoms := r.bottoms[address]
	for i, b := range bottoms {
		if b == mc {
			r.bottoms[address] = append(bottoms[:i], bottoms[i+1:]...)
			util.LogDebug("[REVERSE] unregister: %s", address)
			return true
		}
	}
	return false
}

// PoolSize returns the number of available bottom connections for an address.
func (r *Registry) PoolSize(address string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bottoms[address])
}

// CloseByAddress closes all bottom connections for the given address.
// Used when a control session disconnects to clean up data connections.
func (r *Registry) CloseByAddress(address string) {
	r.mu.Lock()
	bottoms := r.bottoms[address]
	delete(r.bottoms, address)
	// Also wake up any waiters so they return error
	if chs, ok := r.waiters[address]; ok {
		delete(r.waiters, address)
		for _, ch := range chs {
			close(ch)
		}
	}
	r.mu.Unlock()

	for _, mc := range bottoms {
		mc.Stop()
	}
	util.LogInfo("[REVERSE-REGISTRY] closed %d connections for %s", len(bottoms), address)
}

// Match waits for a bottom connection matching the given address.
// Returns a connection that has completed the reverse handshake.
func (r *Registry) Match(address string) (net.Conn, error) {
	r.mu.Lock()

	// Try existing bottoms, skip dead ones
	for len(r.bottoms[address]) > 0 {
		mc := r.bottoms[address][0]
		r.bottoms[address] = r.bottoms[address][1:]
		r.mu.Unlock()

		// Claim ownership immediately so the watcher never races to close.
		mc.matchedMu.Lock()
		mc.matched = true
		mc.matchedMu.Unlock()

		// Perform handshake: send PONG, wait for PENG
		if err := reverseHandshake(mc); err != nil {
			util.LogWarn("[REVERSE] handshake fail for %s, skip dead connection: %v", address, err)
			mc.Conn.Close()
			r.Unregister(address, mc) // remove dead connection from pool
			r.mu.Lock()
			continue
		}
		mc.Stop() // stop readLoop so caller owns the read side
		return mc, nil
	}

	// No bottom available, wait
	ch := make(chan *ManagedConn, 1)
	r.waiters[address] = append(r.waiters[address], ch)
	r.mu.Unlock()

	// Wait with timeout or registry cancellation
	select {
	case mc := <-ch:
		// Channel closed means the registry/caller cancelled the waiter.
		if mc == nil {
			return nil, fmt.Errorf("reverse: match cancelled for %s", address)
		}
		// Claim ownership immediately so the watcher never races to close.
		mc.matchedMu.Lock()
		mc.matched = true
		mc.matchedMu.Unlock()

		if err := reverseHandshake(mc); err != nil {
			mc.Conn.Close()
			r.Unregister(address, mc) // remove dead connection from pool
			return nil, err
		}
		mc.Stop() // stop readLoop so caller owns the read side
		return mc, nil
	case <-r.cancelCh:
		// Registry refreshed — drain any pending value from ch to avoid
		// losing a matched connection, then cancel the waiter.
		select {
		case mc := <-ch:
			// Channel closed means the waiter was already cleaned up.
			if mc == nil {
				return nil, fmt.Errorf("reverse: match cancelled for %s", address)
			}
			r.mu.Lock()
			waiters := r.waiters[address]
			for i, w := range waiters {
				if w == ch {
					r.waiters[address] = append(waiters[:i], waiters[i+1:]...)
					break
				}
			}
			r.mu.Unlock()
			mc.matchedMu.Lock()
			mc.matched = true
			mc.matchedMu.Unlock()
			if err := reverseHandshake(mc); err != nil {
				mc.Conn.Close()
				return nil, err
			}
			return mc, nil
		default:
			r.mu.Lock()
			waiters := r.waiters[address]
			for i, w := range waiters {
				if w == ch {
					r.waiters[address] = append(waiters[:i], waiters[i+1:]...)
					break
				}
			}
			r.mu.Unlock()
			return nil, fmt.Errorf("reverse: match cancelled for %s", address)
		}
	case <-time.After(60 * time.Second):
		// Cancel the waiter
		r.mu.Lock()
		waiters := r.waiters[address]
		for i, w := range waiters {
			if w == ch {
				r.waiters[address] = append(waiters[:i], waiters[i+1:]...)
				break
			}
		}
		r.mu.Unlock()
		return nil, fmt.Errorf("reverse: match timeout for %s", address)
	}
}

// reverseHandshake sends PONG and waits for PENG response.
func reverseHandshake(mc *ManagedConn) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Stop heartbeat sender BEFORE sending PONG. This ensures any extra HEARTBEAT
	// the sender might write is sent before PONG, so the remote side's
	// handleReverseConn loop (still running) will discard it instead of
	// polluting the data stream after handshake.
	mc.stopPingOnce.Do(func() { close(mc.stopPing) })
	mc.senderWg.Wait() // ensure sender has fully exited

	// Send PONG
	if err := mc.WriteMsg(FramePong); err != nil {
		return fmt.Errorf("reverse: send PONG fail: %w", err)
	}

	// Mark matched early so the watcher goroutine sees it before Stop()
	// triggers doneCh close. If the subsequent PENG read fails, Match
	// will close the connection on its error path.
	mc.matchedMu.Lock()
	mc.matched = true
	mc.matchedMu.Unlock()

	// Stop readLoop so we can read directly without racing
	mc.Stop()

	// Check if readLoop already read PENG before it exited
	mc.pendingMu.Lock()
	if mc.hasPending && mc.pendingMsg == FramePeng {
		mc.hasPending = false
		mc.pendingMu.Unlock()
		return nil
	}
	mc.pendingMu.Unlock()

	// Read directly until we get PENG
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("reverse: handshake timeout waiting for PENG")
		}

		mc.Conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		frameType, _, err := ReadFrame(mc.Conn)
		if err != nil {
			return fmt.Errorf("reverse: read PENG fail: %w", err)
		}
		switch frameType {
		case FramePeng:
			mc.Conn.SetReadDeadline(time.Time{})
			return nil
		case FrameHeartbeat:
			continue // discard stray heartbeat
		default:
			util.LogError("[REVERSE] unexpected frame during handshake for %s: 0x%02x", mc.Conn.RemoteAddr(), frameType)
		}
	}
}

// HandleReverseConnection handles a registered bottom connection.
// It registers the connection, starts a heartbeat sender to keep the
// connection alive, and monitors for readLoop exit — if the readLoop
// exits (e.g. due to timeout or connection close), it automatically
// unregisters from the registry and closes the connection.
func HandleReverseConnection(conn net.Conn, address string) {
	registry := GlobalRegistry()
	if registry == nil {
		conn.Close()
		return
	}

	mc := NewManagedConn(conn)
	mc.stopPing = make(chan struct{})

	registry.Register(address, mc)

	// Immediately send PENG to confirm registration, so the client knows
	// the connection is registered and can enter steady heartbeat wait.
	if err := mc.WriteMsg(FramePeng); err != nil {
		util.LogError("[REVERSE] send PENG fail for %s: %v", address, err)
		registry.Unregister(address, mc)
		mc.Stop()
		mc.Conn.Close()
		return
	}

	// Start heartbeat sender: write HEARTBEAT every 10s to keep connection alive.
	// Continues even after matched — the dialer side tcpKeepalive only sends
	// without reading, so both sides keep the TCP connection active.
	mc.senderWg.Add(1)
	go func() {
		defer mc.senderWg.Done()
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := mc.WriteMsg(FrameHeartbeat); err != nil {
					return
				}
			case <-mc.stopPing:
				return
			case <-mc.doneCh:
				return
			}
		}
	}()

	// Reader idle detector: mirrors Java IdleStateHandler readerIdle (60s).
	// If no data received for 60s, force-close the connection.
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				mc.matchedMu.Lock()
				if mc.matched {
					mc.matchedMu.Unlock()
					return
				}
				mc.matchedMu.Unlock()
				mc.lastReadMu.Lock()
				lastRead := mc.lastReadTime
				mc.lastReadMu.Unlock()
				if !lastRead.IsZero() && time.Since(lastRead) > 60*time.Second {
					util.LogWarn("[REVERSE] reader idle timeout for %s", address)
					mc.Conn.Close()
					return
				}
			case <-mc.doneCh:
				return
			case <-mc.stopPing:
				return
			}
		}
	}()

	// Watch for readLoop exit — if it dies (timeout, error), unregister
	// and close the connection so the pool stays clean.
	// But if the connection was already matched (reverseHandshake succeeded),
	// skip cleanup — the connection now belongs to the dialer.
	go func() {
		<-mc.doneCh // blocks until readLoop exits
		mc.matchedMu.Lock()
		matched := mc.matched
		mc.matchedMu.Unlock()
		if matched {
			return
		}
		// If the connection is still in the pool, remove and close it.
		// If Unregister returns false, Match may have just claimed it;
		// recheck matched to avoid racing with reverseHandshake.
		if registry.Unregister(address, mc) {
			mc.Conn.Close()
			return
		}
		mc.matchedMu.Lock()
		matched = mc.matched
		mc.matchedMu.Unlock()
		if !matched {
			mc.Conn.Close()
		}
	}()
}
