package server

import (
	"bytes"
	"io"
	"math"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"phaethon/config"
	"phaethon/dialer"
	"phaethon/frame"
	"phaethon/util"
)

// Mesh channel constants (P2P direct mode over h_tunnel;
// docs/plans/htunnel_p2p_direct_mode.md §6-§7).
const (
	htMeshQueueLen     = 4096             // frames buffered per direction
	htMeshCtrlQueueLen = 256              // control frames buffered toward the client
	htMeshBatchLimit   = 512 * 1024       // one GET/POST batch cap (below nginx 1m)
	htMeshPollTimeout  = 25 * time.Second // GET long-poll wait before 408

	// htMeshMaxQueuedBytes caps data queued for GET delivery; past it the
	// dispatcher stops draining meshOut so Send blocks (§3.9 F2).
	htMeshMaxQueuedBytes = 8 << 20

	// Coalescing window for GET delivery. The p2p writer emits ~64-frame
	// batches (~90KB); flushing to the single data GET loop on every batch
	// caps a channel at roughly 90KB/(GET round trip + transfer) ≈ 2-3MB/s
	// regardless of how many inner flows are active. Holding frames for a
	// couple of milliseconds lets several writer batches clump into one GET
	// response, raising per-response bytes without introducing the
	// cross-GET reordering that multiple concurrent data loops cause.
	htMeshCoalesceWait  = 2 * time.Millisecond
	htMeshCoalesceBytes = 256 * 1024
)

// CoDel (RFC 8289) parameters for the GET delivery queues (§3.9 F2). Target
// leaves room for one GET round trip (~20-40ms) plus batching.
const (
	codelTarget   = 50 * time.Millisecond
	codelInterval = 300 * time.Millisecond
)

var meshEpoch = time.Now()

// meshNow is a monotonic clock for queue sojourn times (never 0 in practice,
// which codel uses as "unset").
func meshNow() time.Duration { return time.Since(meshEpoch) }

// meshMsg is one P2P frame flowing through a mesh channel.
type meshMsg struct {
	frameType byte
	payload   []byte
	control   bool          // p2p control frame: served ahead of all data (§3.8)
	enq       time.Duration // meshNow() when queued for delivery (AQM sojourn)
}

func (m meshMsg) wireSize() int { return 3 + len(m.payload) }

// meshWaiter represents a GET request waiting for frames. The dispatcher
// hands it exactly one batch (batch has cap 1). done is closed under
// ch.dispMu when the handler gives up, so a batch is never handed to a
// waiter whose handler already returned.
type meshWaiter struct {
	batch chan []meshMsg
	done  chan struct{}
	seq   uint64 // server-assigned batch sequence (0 = no data frames)
}

func (w *meshWaiter) stale() bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

// handleMeshChannelRequest serves the MESH channel request (HEAD, X-C: MESH):
// allocates the channel without dialing any target and starts the local P2P
// session directly on the bridged transport (no reverse server splice).
func (s *HTunnelServer) handleMeshChannelRequest(w http.ResponseWriter) {
	id := atomic.AddInt64(&s.idGen, 1)
	connID := util.NextConnID()
	ch := &htChannel{
		id:          id,
		connID:      connID,
		closed:      make(chan struct{}),
		isMesh:      true,
		crypto:      util.NewHTunnelCrypto(s.Password),
		meshIn:      make(chan meshMsg, htMeshQueueLen),
		meshOut:     make(chan meshMsg, htMeshQueueLen),
		meshCtrlOut: make(chan meshMsg, htMeshCtrlQueueLen),
		getWaiters:  make(chan *meshWaiter, 64), // support up to 64 concurrent GETs
		htCfg:       config.HTunnel(),
		postNext:    1,
		postPending: make(map[uint64][]meshMsg),
		postWake:    make(chan struct{}, 1),
	}
	s.channels.Store(id, ch)
	go ch.postDeliveryLoop()

	// The channel is usable immediately (no handshake): reap only if the
	// client never issues a follow-up request; each POST/GET resets the timer.
	ch.mu.Lock()
	ch.reqTimeout = time.AfterFunc(10*time.Second, func() {
		s.closeChannel(id)
	})
	ch.mu.Unlock()

	w.Header().Set(dialer.HeaderConnectionID, strconv.FormatInt(id, 10))
	w.WriteHeader(200)
	util.LogModuleInfo("htunnel", "[%s] [%s] MESH channel started (p2p direct)", s.Mapping.Name, connID)

	go handleP2PTransport(meshChannelTransport{s: s, ch: ch}, "mesh:"+connID)

	// Start dispatcher: distributes meshOut frames to waiting GET requests
	go s.meshDispatchLoop(ch)
}

// meshHandleWrite serves a client POST: decrypt the body, parse the frame
// batch and deliver each frame to the local P2P session via meshIn
// (blocking when the session is slow — backpressure).
func (s *HTunnelServer) meshHandleWrite(ch *htChannel, w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(410)
		return
	}
	if len(data) > 0 {
		data, err = ch.crypto.OpenBody(data)
		if err != nil {
			util.LogModuleWarn("htunnel", "[%s] [%s] mesh POST decrypt fail: %v", s.Mapping.Name, ch.connID, err)
			w.WriteHeader(410)
			return
		}
	}

	msgs := make([]meshMsg, 0, 16)
	for len(data) >= 3 {
		ft, payload, err := frame.ReadFrame(bytes.NewReader(data))
		if err != nil {
			util.LogModuleWarn("htunnel", "[%s] [%s] mesh POST parse fail: %v", s.Mapping.Name, ch.connID, err)
			w.WriteHeader(410)
			return
		}
		msgs = append(msgs, meshMsg{frameType: ft, payload: payload})
		data = data[3+len(payload):]
	}
	seq, _ := strconv.ParseUint(r.Header.Get(dialer.HeaderContentSeq), 10, 64)
	if seq == 0 {
		ch.deliverPost(msgs)
	} else {
		ch.enqueuePost(seq, msgs)
	}
	w.WriteHeader(200)
}

// deliverPost pushes frames into the local P2P session in arrival order.
func (ch *htChannel) deliverPost(msgs []meshMsg) {
	for _, m := range msgs {
		select {
		case ch.meshIn <- m:
		case <-ch.closed:
			return
		}
	}
}

// enqueuePost stores one POST batch by its client write sequence and wakes the
// single delivery goroutine. It never blocks on meshIn, so POST handlers return
// promptly (blocking them caused HTTP timeouts and permanent sequence gaps).
func (ch *htChannel) enqueuePost(seq uint64, msgs []meshMsg) {
	ch.postMu.Lock()
	if seq >= ch.postNext {
		if _, dup := ch.postPending[seq]; !dup {
			ch.postPending[seq] = msgs
			for _, m := range msgs {
				ch.postBytes += 3 + len(m.payload)
			}
		}
	}
	ch.postMu.Unlock()
	select {
	case ch.postWake <- struct{}{}:
	default:
	}
}

// postDeliveryLoop is the only consumer that moves ordered POST batches into
// meshIn. A ticker enforces the gap timeout even when no further POST arrives.
func (ch *htChannel) postDeliveryLoop() {
	const gap = 2 * time.Second
	const maxBytes = 16 << 20
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ch.closed:
			return
		case <-ch.postWake:
		case <-ticker.C:
		}
		ch.skipStalePostGap(gap, maxBytes)
		for {
			ch.postMu.Lock()
			batch, ok := ch.postPending[ch.postNext]
			if ok {
				delete(ch.postPending, ch.postNext)
				ch.postNext++
				for _, m := range batch {
					ch.postBytes -= 3 + len(m.payload)
				}
				ch.postGapAt = time.Time{}
			}
			ch.postMu.Unlock()
			if !ok {
				break
			}
			for _, m := range batch {
				select {
				case ch.meshIn <- m:
				case <-ch.closed:
					return
				}
			}
		}
	}
}

// skipStalePostGap jumps over a missing sequence after gap (or when the reorder
// buffer exceeds maxBytes). The inner TCP retransmits what was actually lost.
func (ch *htChannel) skipStalePostGap(gap time.Duration, maxBytes int) {
	ch.postMu.Lock()
	defer ch.postMu.Unlock()
	if len(ch.postPending) == 0 {
		ch.postGapAt = time.Time{}
		return
	}
	now := time.Now()
	if ch.postGapAt.IsZero() {
		ch.postGapAt = now
		return
	}
	if now.Sub(ch.postGapAt) < gap && ch.postBytes <= maxBytes {
		return
	}
	var min uint64
	first := true
	for s := range ch.postPending {
		if first || s < min {
			min = s
			first = false
		}
	}
	if first || min <= ch.postNext {
		return
	}
	for s, msgs := range ch.postPending {
		if s < min {
			for _, m := range msgs {
				ch.postBytes -= 3 + len(m.payload)
			}
			delete(ch.postPending, s)
		}
	}
	ch.postNext = min
	ch.postGapAt = time.Time{}
}

// meshHandleHeartbeat serves a client PUT for mesh channel heartbeat (v0.2.0).
// Simply resets the request timeout and returns 200.
func (s *HTunnelServer) meshHandleHeartbeat(ch *htChannel, w http.ResponseWriter, r *http.Request) {
	ch.resetReqTimeout(s, ch.id)
	// Heartbeats consume a client write sequence (shared with POSTs); record an
	// empty batch so the reorder gate can advance past it.
	if seq, err := strconv.ParseUint(r.Header.Get(dialer.HeaderContentSeq), 10, 64); err == nil && seq > 0 {
		ch.enqueuePost(seq, nil)
	}
	w.WriteHeader(200)
}

// meshDispatchLoop moves frames from meshOut onto the channel's delivery
// queues and hands batches to registered GET waiters. Exits when the
// channel is closed.
//
// Control frames arrive on their own channel and are always read; data
// reading pauses while htMeshMaxQueuedBytes are queued (§3.9 F2), so a
// stalled client back-pressures the p2p session instead of growing memory.
func (s *HTunnelServer) meshDispatchLoop(ch *htChannel) {
	limit := ch.htCfg.GetMaxBatchSize()
	var lastDrops uint64
	var lastDropLog time.Time

	// Coalescing timer: data is flushed immediately when it is already large
	// enough (or when control frames are queued); otherwise the timer gives
	// other writer batches a 2ms window to join the same GET response.
	var flushTimer *time.Timer
	var flushC <-chan time.Time
	stopTimer := func() {
		if flushTimer != nil {
			if !flushTimer.Stop() {
				select {
				case <-flushTimer.C:
				default:
				}
			}
			flushTimer = nil
			flushC = nil
		}
	}
	armTimer := func() {
		if flushTimer == nil {
			flushTimer = time.NewTimer(htMeshCoalesceWait)
			flushC = flushTimer.C
		}
	}
	defer stopTimer()

	for {
		in := ch.meshOut
		if ch.dataBytes >= htMeshMaxQueuedBytes {
			in = nil
		}
		select {
		case msg := <-ch.meshCtrlOut:
			ch.dispMu.Lock()
			ch.routeFrameLocked(msg)
			ch.drainLocked()
			ch.flushLocked(limit)
			ch.dispMu.Unlock()
			stopTimer()
		case msg := <-in:
			ch.dispMu.Lock()
			ch.routeFrameLocked(msg)
			// The dispatcher is the only consumer of meshOut/meshCtrlOut, so
			// draining the current backlog cannot block; it lets one flush
			// build full batches under load while an idle channel still
			// ships the first frame immediately.
			ch.drainLocked()
			if len(ch.ctrlQ) > 0 || ch.dataBytes >= htMeshCoalesceBytes {
				ch.flushLocked(limit)
				ch.dispMu.Unlock()
				stopTimer()
			} else {
				ch.dispMu.Unlock()
				armTimer()
			}
		case w := <-ch.getWaiters:
			ch.dispMu.Lock()
			ch.addWaiterLocked(w)
			for n := len(ch.getWaiters); n > 0; n-- {
				ch.addWaiterLocked(<-ch.getWaiters)
			}
			if len(ch.ctrlQ) > 0 || ch.dataBytes >= htMeshCoalesceBytes || len(ch.dataQ) == 0 {
				// Control frames must not wait; a full coalesced batch is
				// ready; or there is nothing to send yet (the timer armed by
				// the next data frame will flush it).
				ch.flushLocked(limit)
				ch.dispMu.Unlock()
				stopTimer()
			} else {
				ch.dispMu.Unlock()
				armTimer()
			}
		case <-flushC:
			ch.dispMu.Lock()
			ch.drainLocked()
			ch.flushLocked(limit)
			ch.dispMu.Unlock()
			stopTimer()
		case <-ch.closed:
			return
		}
		if ch.codelDrops != lastDrops && time.Since(lastDropLog) >= 10*time.Second {
			util.LogModuleInfo("htunnel", "[%s] [%s] mesh AQM dropped %d frames (total %d), queued=%dB",
				s.Mapping.Name, ch.connID, ch.codelDrops-lastDrops, ch.codelDrops, ch.dataBytes)
			lastDrops, lastDropLog = ch.codelDrops, time.Now()
		}
	}
}

// drainLocked moves the frames already buffered in meshCtrlOut and meshOut
// onto the delivery queues; data stops at the byte cap. Caller holds dispMu.
func (ch *htChannel) drainLocked() {
	for n := len(ch.meshCtrlOut); n > 0; n-- {
		ch.routeFrameLocked(<-ch.meshCtrlOut)
	}
	for n := len(ch.meshOut); n > 0 && ch.dataBytes < htMeshMaxQueuedBytes; n-- {
		ch.routeFrameLocked(<-ch.meshOut)
	}
}

// routeFrameLocked appends a frame to its delivery queue. Control frames go
// to ctrlQ (served first to any waiter: a starved pong gets the session
// declared link-dead by the peer, §3.8); everything else, pings included,
// rides the data FIFO so pings still measure the data path.
// Caller holds ch.dispMu.
func (ch *htChannel) routeFrameLocked(msg meshMsg) {
	if msg.control {
		ch.ctrlQ = append(ch.ctrlQ, msg)
		return
	}
	msg.enq = meshNow()
	ch.dataBytes += msg.wireSize()
	ch.dataQ = append(ch.dataQ, msg)
}

// addWaiterLocked registers a GET waiter. Caller holds ch.dispMu.
func (ch *htChannel) addWaiterLocked(w *meshWaiter) {
	ch.waiters = append(pruneStale(ch.waiters), w)
}

// recordSojournLocked folds one delivered frame's queue time into the
// server-side stats. Caller holds ch.dispMu.
func (ch *htChannel) recordSojournLocked(m meshMsg) {
	if m.enq == 0 {
		return
	}
	s := meshNow() - m.enq
	ch.statFrames++
	ch.statSum += s
	if s > ch.statMax {
		ch.statMax = s
	}
}

// maybeLogSojournLocked emits the 5s server-side queue summary. Caller holds
// ch.dispMu.
func (ch *htChannel) maybeLogSojournLocked() {
	if ch.statFrames == 0 || time.Since(ch.statLastLog) < 5*time.Second {
		return
	}
	avg := ch.statSum / time.Duration(ch.statFrames)
	util.LogModuleInfo("htunnel", "[HT-SERVER-STATS] conn=%s frames=%d avgSojourn=%.1fms maxSojourn=%.1fms queued=%d waiters=%d",
		ch.connID, ch.statFrames, float64(avg)/1e6, float64(ch.statMax)/1e6,
		ch.dataBytes, len(ch.waiters))
	ch.statFrames, ch.statSum, ch.statMax = 0, 0, 0
	ch.statLastLog = time.Now()
}

// flushLocked hands one batch to every waiter while frames are queued.
// Caller holds ch.dispMu.
func (ch *htChannel) flushLocked(limit int) {
	ch.waiters = pruneStale(ch.waiters)
	for len(ch.waiters) > 0 && (len(ch.ctrlQ) > 0 || len(ch.dataQ) > 0) {
		w := ch.waiters[0]
		ch.waiters = popWaiter(ch.waiters)
		if !w.stale() {
			batch := ch.buildBatchLocked(limit)
			w.seq = 0
			for _, m := range batch {
				if !m.control {
					ch.nextBatchSeq++
					w.seq = ch.nextBatchSeq
					break
				}
			}
			w.batch <- batch
		}
	}
	ch.maybeLogSojournLocked()
}

// buildBatchLocked assembles one GET batch of at most limit wire bytes (at
// least one frame): control frames first, then data. Caller holds ch.dispMu.
func (ch *htChannel) buildBatchLocked(limit int) []meshMsg {
	var batch []meshMsg
	size := 0
	batch, ch.ctrlQ = takeFrames(batch, &size, ch.ctrlQ, limit)
	batch, ch.dataQ = ch.takeData(batch, &size, ch.dataQ, &ch.dataCodel, limit, meshNow())
	return batch
}

// takeData is takeFrames for a data queue: dequeued frames leave dataBytes,
// and mesh packets the CoDel state machine marks are dropped instead of
// delivered (sparse head drops: the inner TCP sees one loss and fast
// retransmits, shrinking its window before the standing queue grows).
func (ch *htChannel) takeData(batch []meshMsg, size *int, q []meshMsg, cd *codel, limit int, now time.Duration) ([]meshMsg, []meshMsg) {
	n := 0
	for n < len(q) {
		m := q[n]
		sz := m.wireSize()
		if len(batch) > 0 && *size+sz > limit {
			break
		}
		n++
		ch.dataBytes -= sz
		if cd.shouldDrop(now-m.enq, now, len(q)-n) && m.frameType == frame.FrameMeshPacket {
			ch.codelDrops++
			continue
		}
		batch = append(batch, m)
		*size += sz
		ch.recordSojournLocked(m)
	}
	clear(q[:n])
	if n == len(q) {
		return batch, nil
	}
	return batch, q[n:]
}

// codel is the RFC 8289 state of one delivery queue, evaluated per dequeued
// frame.
type codel struct {
	firstAbove time.Duration // 0 = sojourn currently below target
	dropNext   time.Duration
	count      int
	lastCount  int
	dropping   bool
}

func codelControlLaw(t time.Duration, count int) time.Duration {
	return t + time.Duration(float64(codelInterval)/math.Sqrt(float64(count)))
}

// okToDrop is RFC 8289 dodequeue: true once sojourn has stayed above target
// for a full interval. backlog is the frames left behind this one; a nearly
// empty queue is never dropped from.
func (c *codel) okToDrop(sojourn, now time.Duration, backlog int) bool {
	if sojourn < codelTarget || backlog < 2 {
		c.firstAbove = 0
		return false
	}
	if c.firstAbove == 0 {
		c.firstAbove = now + codelInterval
		return false
	}
	return now >= c.firstAbove
}

// shouldDrop runs the RFC 8289 dequeue state machine for one frame.
func (c *codel) shouldDrop(sojourn, now time.Duration, backlog int) bool {
	ok := c.okToDrop(sojourn, now, backlog)
	if c.dropping {
		if !ok {
			c.dropping = false
			return false
		}
		if now >= c.dropNext {
			c.count++
			c.dropNext = codelControlLaw(c.dropNext, c.count)
			return true
		}
		return false
	}
	if !ok {
		return false
	}
	c.dropping = true
	delta := c.count - c.lastCount
	if delta > 1 && now-c.dropNext < 16*codelInterval {
		c.count = delta
	} else {
		c.count = 1
	}
	c.dropNext = codelControlLaw(now, c.count)
	c.lastCount = c.count
	return true
}

// takeFrames moves frames from the head of q into batch while the batch
// stays within limit bytes (an empty batch always takes one frame). Taken
// slots are zeroed so the backing array does not pin delivered payloads.
func takeFrames(batch []meshMsg, size *int, q []meshMsg, limit int) ([]meshMsg, []meshMsg) {
	n := 0
	for n < len(q) {
		sz := q[n].wireSize()
		if len(batch) > 0 && *size+sz > limit {
			break
		}
		batch = append(batch, q[n])
		*size += sz
		n++
	}
	clear(q[:n])
	if n == len(q) {
		return batch, nil
	}
	return batch, q[n:]
}

// pruneStale drops abandoned waiters from the front of a list. Waiters time
// out roughly in registration order, so front pruning bounds idle growth.
func pruneStale(ws []*meshWaiter) []*meshWaiter {
	for len(ws) > 0 && ws[0].stale() {
		ws = popWaiter(ws)
	}
	return ws
}

func popWaiter(ws []*meshWaiter) []*meshWaiter {
	ws[0] = nil
	if len(ws) == 1 {
		return nil
	}
	return ws[1:]
}

// abandonWaiter marks w stale under dispMu, then returns a batch the
// dispatcher handed off just before (nil if none), so frames are never lost
// in the timeout race.
func (ch *htChannel) abandonWaiter(w *meshWaiter) []meshMsg {
	ch.dispMu.Lock()
	close(w.done)
	ch.dispMu.Unlock()
	select {
	case b := <-w.batch:
		return b
	default:
		return nil
	}
}

// meshHandleRead serves a client long-poll GET: register as a waiter, wait up
// to the client-specified wait time (X-W header, default 5s) for a batch from
// the dispatcher (408 on timeout) and return it encrypted.
func (s *HTunnelServer) meshHandleRead(ch *htChannel, w http.ResponseWriter, r *http.Request) {
	// Read client-controlled wait time (v0.2.0)
	waitTime := 5 * time.Second
	if waitTimeStr := r.Header.Get(dialer.HeaderWaitTime); waitTimeStr != "" {
		if waitTimeSec, err := strconv.Atoi(waitTimeStr); err == nil && waitTimeSec > 0 {
			waitTime = time.Duration(waitTimeSec) * time.Second
			// Clamp to reasonable range
			minWait, maxWait, _ := ch.htCfg.GetWaitTimeRange()
			if waitTime < minWait {
				waitTime = minWait
			}
			if waitTime > maxWait {
				waitTime = maxWait
			}
		}
	}

	// The dispatcher hands this waiter exactly one batch, already sized by
	// bytes (≤ MaxBatchSize) with control frames first (§3.8).
	waiter := &meshWaiter{
		batch: make(chan []meshMsg, 1),
		done:  make(chan struct{}),
	}

	// Register as a waiter. On timeout here the dispatcher never saw it.
	select {
	case ch.getWaiters <- waiter:
	case <-time.After(waitTime):
		w.WriteHeader(408)
		return
	case <-ch.closed:
		w.WriteHeader(410)
		return
	}

	timer := time.NewTimer(waitTime)
	defer timer.Stop()
	var batch []meshMsg
	select {
	case batch = <-waiter.batch:
	case <-timer.C:
		if batch = ch.abandonWaiter(waiter); batch == nil {
			w.WriteHeader(408)
			return
		}
	case <-ch.closed:
		ch.abandonWaiter(waiter)
		w.WriteHeader(410)
		return
	}

	var buf bytes.Buffer
	for _, msg := range batch {
		_ = frame.WriteFrame(&buf, msg.frameType, msg.payload)
	}
	w.WriteHeader(200)
	w.Write(ch.crypto.SealBody(buf.Bytes()))
}

// meshChannelTransport bridges a mesh channel to frame.FrameTransport for the
// local P2P session. Send pushes toward the client (drained by GET long-poll);
// Recv pulls frames POSTed by the client. Both block with backpressure and
// unblock via ch.closed. The session owns the channel's lifetime from this
// side: Close tears the channel down so the client re-dials at once (410)
// instead of polling a channel nobody serves (§3.9 F2).
type meshChannelTransport struct {
	s  *HTunnelServer
	ch *htChannel
}

func (t meshChannelTransport) Send(frameType byte, payload []byte, isControl bool) error {
	out := t.ch.meshOut
	if isControl {
		out = t.ch.meshCtrlOut
	}
	select {
	case out <- meshMsg{frameType: frameType, payload: payload, control: isControl}:
		return nil
	case <-t.ch.closed:
		return io.ErrClosedPipe
	}
}

// SendBatch sends multiple data frames to the client (p2p batches only data;
// control frames go through Send). Each frame is enqueued individually; the
// dispatcher re-batches them per GET.
func (t meshChannelTransport) SendBatch(frames []frame.Frame) error {
	for _, f := range frames {
		out := t.ch.meshOut
		if f.Control {
			out = t.ch.meshCtrlOut
		}
		select {
		case out <- meshMsg{frameType: f.Type, payload: f.Payload, control: f.Control}:
		case <-t.ch.closed:
			return io.ErrClosedPipe
		}
	}
	return nil
}

func (t meshChannelTransport) Recv() (byte, []byte, error) {
	select {
	case msg := <-t.ch.meshIn:
		return msg.frameType, msg.payload, nil
	case <-t.ch.closed:
		return 0, nil, io.ErrClosedPipe
	}
}

func (t meshChannelTransport) Close() error {
	t.s.closeChannel(t.ch.id)
	return nil
}
