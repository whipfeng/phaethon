package server

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"phaethon/dialer"
	"phaethon/frame"
	"phaethon/util"
)

// Mesh channel constants (P2P direct mode over h_tunnel;
// docs/plans/htunnel_p2p_direct_mode.md §6-§7).
const (
	htMeshQueueLen    = 4096             // frames buffered per direction
	htMeshBatchLimit  = 512 * 1024       // one GET/POST batch cap (below nginx 1m)
	htMeshPollTimeout = 25 * time.Second // GET long-poll wait before 408
)

// meshMsg is one P2P frame flowing through a mesh channel.
type meshMsg struct {
	frameType byte
	payload   []byte
	flowKey   uint32 // inner-flow key (frame.FlowKey); flowKeyAny = no pinning
	control   bool   // p2p control frame: served ahead of all data (§3.8)
}

func (m meshMsg) wireSize() int { return 3 + len(m.payload) }

// flowKeyAny marks data frames that carry no inner flow (e.g. link pings):
// spread round-robin over the pipes so they still ride the data path.
const flowKeyAny = ^uint32(0)

// meshWaiter represents a GET request waiting for frames. The dispatcher
// hands it exactly one batch (batch has cap 1). done is closed under
// ch.pipeMu when the handler gives up, so a batch is never handed to a
// waiter whose handler already returned.
// pipe >= 0: the waiter receives control frames plus its pipe's data.
// pipe == -1: wildcard — control frames plus the wildcard queue.
type meshWaiter struct {
	batch chan []meshMsg
	done  chan struct{}
	pipe  int // pipe index announced via X-Q, or -1
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
		id:         id,
		connID:     connID,
		closed:     make(chan struct{}),
		isMesh:     true,
		crypto:     util.NewHTunnelCrypto(s.Password),
		meshIn:     make(chan meshMsg, htMeshQueueLen),
		meshOut:    make(chan meshMsg, htMeshQueueLen),
		getWaiters: make(chan *meshWaiter, 64), // support up to 64 concurrent GETs
	}
	s.channels.Store(id, ch)

	// Per-flow ordered GET dispatch state (§3.2); pipe slices grow lazily
	// when the client announces its pipe count via X-N.
	ch.pipeQ = make([][]meshMsg, 0, 8)
	ch.pipeWait = make([][]*meshWaiter, 0, 8)
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

	go handleP2PTransport(meshChannelTransport{ch: ch}, "mesh:"+connID)

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

	for len(data) >= 3 {
		ft, payload, err := frame.ReadFrame(bytes.NewReader(data))
		if err != nil {
			util.LogModuleWarn("htunnel", "[%s] [%s] mesh POST parse fail: %v", s.Mapping.Name, ch.connID, err)
			w.WriteHeader(410)
			return
		}
		select {
		case ch.meshIn <- meshMsg{frameType: ft, payload: payload}:
		case <-ch.closed:
			w.WriteHeader(410)
			return
		}
		data = data[3+len(payload):]
	}
	w.WriteHeader(200)
}

// meshHandleHeartbeat serves a client PUT for mesh channel heartbeat (v0.2.0).
// Simply resets the request timeout and returns 200.
func (s *HTunnelServer) meshHandleHeartbeat(ch *htChannel, w http.ResponseWriter, r *http.Request) {
	ch.resetReqTimeout(s, ch.id)
	w.WriteHeader(200)
}

// meshDispatchLoop moves frames from meshOut onto the channel's delivery
// queues and hands batches to registered GET waiters. It never blocks on an
// idle pipe, so a pipe without a registered GET cannot head-of-line block
// other pipes. Exits when the channel is closed.
//
// Ordered vs legacy (AllowFlowReorder) is decided by config alone, never by
// channel state at startup: the dispatcher starts before the client's first
// GET announces the pipe count (X-N), so deciding on pipeCnt here would lock
// every channel into legacy mode (found 2026-11-06, §3.7). In legacy mode
// meshHandleRead ignores X-N, pipeCnt stays 0 and every data frame rides the
// single wildcard FIFO — the same loop serves both modes.
func (s *HTunnelServer) meshDispatchLoop(ch *htChannel) {
	limit := s.htConfig.GetMaxBatchSize()
	for {
		select {
		case msg := <-ch.meshOut:
			ch.pipeMu.Lock()
			ch.routeFrameLocked(msg)
			// The dispatcher is meshOut's only consumer, so draining the
			// current backlog cannot block; it lets one flush build full
			// batches under load while an idle channel still ships the
			// first frame immediately.
			for n := len(ch.meshOut); n > 0; n-- {
				ch.routeFrameLocked(<-ch.meshOut)
			}
			ch.flushLocked(limit)
			ch.pipeMu.Unlock()
		case w := <-ch.getWaiters:
			ch.pipeMu.Lock()
			ch.addWaiterLocked(w)
			for n := len(ch.getWaiters); n > 0; n-- {
				ch.addWaiterLocked(<-ch.getWaiters)
			}
			ch.flushLocked(limit)
			ch.pipeMu.Unlock()
		case <-ch.closed:
			return
		}
	}
}

// routeFrameLocked appends a frame to its delivery queue. Control frames go
// to ctrlQ (served first to any waiter: a starved pong gets the session
// declared link-dead by the peer, §3.8). Data waits on its flow's pipe;
// unkeyed data (pings) is spread round-robin so it still measures the data
// path. Before X-N (pipeCnt == 0) data rides the wildcard queue.
// Queues are bounded indirectly by the meshOut backpressure at the sender.
// Caller holds ch.pipeMu.
func (ch *htChannel) routeFrameLocked(msg meshMsg) {
	switch {
	case msg.control:
		ch.ctrlQ = append(ch.ctrlQ, msg)
	case ch.pipeCnt == 0:
		ch.pipeAnyQ = append(ch.pipeAnyQ, msg)
	case msg.flowKey == flowKeyAny:
		p := ch.pipeRR % ch.pipeCnt
		ch.pipeRR = p + 1
		ch.pipeQ[p] = append(ch.pipeQ[p], msg)
	default:
		p := int(msg.flowKey % uint32(ch.pipeCnt))
		ch.pipeQ[p] = append(ch.pipeQ[p], msg)
	}
}

// addWaiterLocked registers a GET waiter on its pipe, or as wildcard when its
// pipe is unknown. Caller holds ch.pipeMu.
func (ch *htChannel) addWaiterLocked(w *meshWaiter) {
	if w.pipe >= 0 && w.pipe < ch.pipeCnt {
		ch.pipeWait[w.pipe] = append(pruneStale(ch.pipeWait[w.pipe]), w)
		return
	}
	w.pipe = -1
	ch.pipeAnyW = append(pruneStale(ch.pipeAnyW), w)
}

// flushLocked hands one batch to every waiter that has deliverable frames.
// Caller holds ch.pipeMu.
func (ch *htChannel) flushLocked(limit int) {
	for p := range ch.pipeWait {
		ch.pipeWait[p] = pruneStale(ch.pipeWait[p])
		for len(ch.pipeWait[p]) > 0 && (len(ch.ctrlQ) > 0 || len(ch.pipeAnyQ) > 0 || len(ch.pipeQ[p]) > 0) {
			w := ch.pipeWait[p][0]
			ch.pipeWait[p] = popWaiter(ch.pipeWait[p])
			if !w.stale() {
				w.batch <- ch.buildBatchLocked(p, limit)
			}
		}
	}
	ch.pipeAnyW = pruneStale(ch.pipeAnyW)
	for len(ch.pipeAnyW) > 0 && (len(ch.ctrlQ) > 0 || len(ch.pipeAnyQ) > 0) {
		w := ch.pipeAnyW[0]
		ch.pipeAnyW = popWaiter(ch.pipeAnyW)
		if !w.stale() {
			w.batch <- ch.buildBatchLocked(-1, limit)
		}
	}
}

// buildBatchLocked assembles one GET batch of at most limit wire bytes (at
// least one frame): control frames, then the wildcard queue (data queued
// before X-N, older than any pipe data), then the waiter's pipe (p >= 0).
// Caller holds ch.pipeMu.
func (ch *htChannel) buildBatchLocked(p, limit int) []meshMsg {
	var batch []meshMsg
	size := 0
	batch, ch.ctrlQ = takeFrames(batch, &size, ch.ctrlQ, limit)
	batch, ch.pipeAnyQ = takeFrames(batch, &size, ch.pipeAnyQ, limit)
	if p >= 0 {
		batch, ch.pipeQ[p] = takeFrames(batch, &size, ch.pipeQ[p], limit)
	}
	return batch
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

// abandonWaiter marks w stale under pipeMu, then returns a batch the
// dispatcher handed off just before (nil if none), so frames are never lost
// in the timeout race.
func (ch *htChannel) abandonWaiter(w *meshWaiter) []meshMsg {
	ch.pipeMu.Lock()
	close(w.done)
	ch.pipeMu.Unlock()
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
			minWait, maxWait, _ := s.htConfig.GetWaitTimeRange()
			if waitTime < minWait {
				waitTime = minWait
			}
			if waitTime > maxWait {
				waitTime = maxWait
			}
		}
	}

	// Per-flow ordered delivery: the client announces its GET pipe count
	// (X-N) and this request's pipe (X-Q). A piped GET only ever receives
	// frames of flows hashed onto its pipe, keeping each flow on one serial
	// GET chain. GETs without valid pipe headers stay legacy wildcard.
	pipe := -1
	if !s.htConfig.GetAllowFlowReorder() {
		if npStr := r.Header.Get(dialer.HeaderPipeCount); npStr != "" {
			if np, err := strconv.Atoi(npStr); err == nil && np > 0 && np <= 64 {
				ch.pipeMu.Lock()
				if np > ch.pipeCnt {
					for len(ch.pipeQ) < np {
						ch.pipeQ = append(ch.pipeQ, nil)
						ch.pipeWait = append(ch.pipeWait, nil)
					}
					ch.pipeCnt = np
				}
				if piStr := r.Header.Get(dialer.HeaderPipeIndex); piStr != "" {
					if pi, err := strconv.Atoi(piStr); err == nil && pi >= 0 && pi < ch.pipeCnt {
						pipe = pi
					}
				}
				ch.pipeMu.Unlock()
			}
		}
	}

	// The dispatcher hands this waiter exactly one batch, already sized by
	// bytes (≤ MaxBatchSize) with control frames first (§3.8).
	waiter := &meshWaiter{
		batch: make(chan []meshMsg, 1),
		done:  make(chan struct{}),
		pipe:  pipe,
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
// Recv pulls frames POSTed by the client. Both block with backpressure; the
// channel timeout/closeChannel path unblocks them via ch.closed.
type meshChannelTransport struct {
	ch *htChannel
}

// flowKeyOf returns the inner-flow key for per-flow ordered delivery:
// mesh packets are keyed by their IPv4 5-tuple hash; other frames carry no
// inner flow (control frames are classified by isControl instead).
func flowKeyOf(frameType byte, payload []byte) uint32 {
	if frameType != frame.FrameMeshPacket {
		return flowKeyAny
	}
	return frame.FlowKey(payload)
}

func (t meshChannelTransport) Send(frameType byte, payload []byte, isControl bool) error {
	select {
	case t.ch.meshOut <- meshMsg{frameType: frameType, payload: payload, flowKey: flowKeyOf(frameType, payload), control: isControl}:
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
		select {
		case t.ch.meshOut <- meshMsg{frameType: f.Type, payload: f.Payload, flowKey: flowKeyOf(f.Type, f.Payload)}:
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
	// Channel lifecycle is owned by closeChannel (client DELETE or timeout).
	return nil
}
