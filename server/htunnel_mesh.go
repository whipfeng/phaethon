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
}

// flowKeyAny marks frames that are not pinned to a pipe: control frames
// and data from legacy/unordered clients. Routed to any available waiter.
const flowKeyAny = ^uint32(0)

// meshWaiter represents a GET request waiting for frames.
// The done channel is closed when the handler times out, signaling
// the dispatcher to skip this waiter.
// pipe >= 0: the waiter only receives frames of its pipe (per-flow ordered
// delivery). pipe == -1: wildcard — receives control frames and, when the
// channel is in legacy mode (pipeCnt == 0), any frame.
type meshWaiter struct {
	msg  chan meshMsg  // frames delivered by dispatcher (buffered for coalescing)
	done chan struct{} // closed on handler timeout → waiter is stale
	pipe int           // pipe index announced via X-Q, or -1
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

// meshDispatchLoop distributes frames from meshOut to GET requests.
// Two modes:
//   - pipeCnt == 0 (legacy): frames go to the first available waiter and
//     GET handlers coalesce by draining meshOut directly.
//   - pipeCnt > 0 (per-flow ordered, client announced X-N): each frame is
//     routed onto its flow's pipe queue (flowKey % pipeCnt) and served
//     only to a waiter of the same pipe — one inner flow rides one serial
//     GET chain and arrives client-side in emission order. Wildcard
//     frames (control, legacy data) go to pipeAnyQ and ride any waiter.
func (s *HTunnelServer) meshDispatchLoop(ch *htChannel) {
	if s.legacyDispatch(ch) {
		s.legacyDispatchLoop(ch)
		return
	}
	s.pipeDispatchLoop(ch)
}

// legacyDispatch reports whether the channel is still in unordered mode
// (no client pipe announcement yet, or the server has flow reordering
// explicitly allowed).
func (s *HTunnelServer) legacyDispatch(ch *htChannel) bool {
	ch.pipeMu.Lock()
	defer ch.pipeMu.Unlock()
	return ch.pipeCnt == 0 || s.htConfig.GetAllowFlowReorder()
}

func (s *HTunnelServer) legacyDispatchLoop(ch *htChannel) {
	for {
		select {
		case msg := <-ch.meshOut:
			if !s.dispatchToWaiter(ch, msg) {
				return
			}
		case <-ch.closed:
			return
		}
	}
}

// pipeDispatchLoop routes every meshOut frame onto a per-pipe queue and
// flushes queues to same-pipe waiters. Never blocks on an idle pipe, so a
// pipe without a registered GET cannot head-of-line block other pipes.
// Exits when the channel is closed.
func (s *HTunnelServer) pipeDispatchLoop(ch *htChannel) {
	for {
		if !s.flushPipes(ch) {
			return
		}
		select {
		case msg := <-ch.meshOut:
			s.routeToPipe(ch, msg)
		case w := <-ch.getWaiters:
			ch.pipeMu.Lock()
			if w.pipe >= 0 && w.pipe < ch.pipeCnt {
				ch.pipeWait[w.pipe] = append(ch.pipeWait[w.pipe], w)
			} else {
				w.pipe = -1
				ch.pipeAnyW = append(ch.pipeAnyW, w)
			}
			ch.pipeMu.Unlock()
		case <-ch.closed:
			return
		}
	}
}

// routeToPipe appends a frame to its pipe queue (or the wildcard queue).
// Queues are bounded indirectly by the meshOut backpressure at the sender.
func (s *HTunnelServer) routeToPipe(ch *htChannel, msg meshMsg) {
	ch.pipeMu.Lock()
	defer ch.pipeMu.Unlock()
	if msg.flowKey == flowKeyAny || ch.pipeCnt == 0 {
		ch.pipeAnyQ = append(ch.pipeAnyQ, msg)
		return
	}
	p := int(msg.flowKey % uint32(ch.pipeCnt))
	ch.pipeQ[p] = append(ch.pipeQ[p], msg)
}

// flushPipes delivers queued frames to waiters of the same pipe (or any
// waiter for the wildcard queue). Stale (timed-out) waiters are dropped.
// Returns false when the channel is closed.
func (s *HTunnelServer) flushPipes(ch *htChannel) bool {
	ch.pipeMu.Lock()
	defer ch.pipeMu.Unlock()

	// dropStale removes timed-out waiters from the front of a list.
	dropStale := func(ws []*meshWaiter) []*meshWaiter {
		for len(ws) > 0 {
			select {
			case <-ws[0].done:
				ws = ws[1:]
			default:
				return ws
			}
		}
		return ws
	}
	// deliver pushes up to cap(w.msg) queued frames into one waiter.
	deliver := func(q []meshMsg, w *meshWaiter) []meshMsg {
		for len(q) > 0 && len(w.msg) < cap(w.msg) {
			w.msg <- q[0]
			q = q[1:]
		}
		return q
	}

	for p := range ch.pipeQ {
		ch.pipeWait[p] = dropStale(ch.pipeWait[p])
		for len(ch.pipeQ[p]) > 0 && len(ch.pipeWait[p]) > 0 {
			w := ch.pipeWait[p][0]
			ch.pipeQ[p] = deliver(ch.pipeQ[p], w)
			// Waiter is consumed by its GET handler; remaining frames wait
			// for the client's next GET on this pipe.
			ch.pipeWait[p] = ch.pipeWait[p][1:]
			ch.pipeWait[p] = dropStale(ch.pipeWait[p])
		}
	}

	ch.pipeAnyW = dropStale(ch.pipeAnyW)
	for len(ch.pipeAnyQ) > 0 {
		if len(ch.pipeAnyW) > 0 {
			w := ch.pipeAnyW[0]
			ch.pipeAnyQ = deliver(ch.pipeAnyQ, w)
			ch.pipeAnyW = ch.pipeAnyW[1:]
			ch.pipeAnyW = dropStale(ch.pipeAnyW)
			continue
		}
		// No wildcard waiter: control frames may borrow any pipe waiter.
		borrowed := false
		for p := range ch.pipeQ {
			ch.pipeWait[p] = dropStale(ch.pipeWait[p])
			if len(ch.pipeWait[p]) > 0 {
				w := ch.pipeWait[p][0]
				ch.pipeAnyQ = deliver(ch.pipeAnyQ, w)
				ch.pipeWait[p] = ch.pipeWait[p][1:]
				borrowed = true
				break
			}
		}
		if !borrowed {
			break
		}
	}
	select {
	case <-ch.closed:
		return false
	default:
		return true
	}
}

// dispatchToWaiter sends msg to the next available GET waiter (legacy mode).
// If the waiter has timed out (done closed), the frame stays in meshOut
// for the next waiter to pick up.
func (s *HTunnelServer) dispatchToWaiter(ch *htChannel, msg meshMsg) bool {
	for {
		select {
		case waiter := <-ch.getWaiters:
			select {
			case waiter.msg <- msg:
				return true
			case <-waiter.done:
				// Waiter timed out, try next waiter (frame stays in meshOut)
				continue
			case <-ch.closed:
				return false
			}
		case <-ch.closed:
			return false
		}
	}
}

// meshHandleRead serves a client long-poll GET: register as a waiter, wait up
// to client-specified wait time (X-W header, default 5s) for a frame from the
// dispatcher (408 on timeout), coalesce further queued frames into one batch
// (≤maxBatchSize) and return it encrypted.
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

	// Get max batch size from config
	maxBatchSize := s.htConfig.GetMaxBatchSize()

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

	// Create a waiter with msg and done channels. The dispatcher exclusively
	// owns meshOut and pushes up to cap(msg) frames into each waiter.
	waiter := &meshWaiter{
		msg:  make(chan meshMsg, 32),
		done: make(chan struct{}),
		pipe: pipe,
	}

	// Register as a waiter
	select {
	case ch.getWaiters <- waiter:
	case <-time.After(waitTime):
		w.WriteHeader(408)
		return
	case <-ch.closed:
		w.WriteHeader(410)
		return
	}

	// Wait for the first frame from the dispatcher
	var buf bytes.Buffer
	select {
	case msg := <-waiter.msg:
		_ = frame.WriteFrame(&buf, msg.frameType, msg.payload)
	case <-time.After(waitTime):
		// Signal dispatcher that this waiter is stale
		close(waiter.done)
		w.WriteHeader(408)
		return
	case <-ch.closed:
		close(waiter.done)
		w.WriteHeader(410)
		return
	}

	// Coalesce additional frames the dispatcher has already pushed into this
	// waiter's msg buffer (non-blocking). Piped waiters only ever receive
	// frames of their pipe, preserving per-flow order; wildcard waiters
	// receive whatever the dispatcher routed (legacy mode: everything).
	for buf.Len() < maxBatchSize {
		done := false
		select {
		case msg := <-waiter.msg:
			_ = frame.WriteFrame(&buf, msg.frameType, msg.payload)
		default:
			done = true
		}
		if done {
			break
		}
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
// mesh packets are keyed by their IPv4 5-tuple hash, everything else
// (control frames, legacy/unparseable payloads) is routed wildcard.
func flowKeyOf(frameType byte, payload []byte) uint32 {
	if frameType != frame.FrameMeshPacket {
		return flowKeyAny
	}
	return frame.FlowKey(payload)
}

func (t meshChannelTransport) Send(frameType byte, payload []byte, isControl bool) error {
	select {
	case t.ch.meshOut <- meshMsg{frameType: frameType, payload: payload, flowKey: flowKeyOf(frameType, payload)}:
		return nil
	case <-t.ch.closed:
		return io.ErrClosedPipe
	}
}

// SendBatch sends multiple frames to the client. Each frame is enqueued
// individually; the GET long-poll coalesces them on the response path.
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
