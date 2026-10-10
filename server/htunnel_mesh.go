package server

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

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
}

// meshWaiter represents a GET request waiting for a frame.
// The done channel is closed when the handler times out, signaling
// the dispatcher to skip this waiter and try the next one.
type meshWaiter struct {
	msg  chan meshMsg   // frame delivered by dispatcher
	done chan struct{}  // closed on handler timeout → waiter is stale
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

	// The channel is usable immediately (no handshake): reap only if the
	// client never issues a follow-up request; each POST/GET resets the timer.
	ch.mu.Lock()
	ch.reqTimeout = time.AfterFunc(10*time.Second, func() {
		s.closeChannel(id)
	})
	ch.mu.Unlock()

	w.Header().Set(htHeaderConnectionID, strconv.FormatInt(id, 10))
	w.WriteHeader(200)
	util.LogInfo("[HT-SVR] [%s] [%s] MESH channel started (p2p direct)", s.Mapping.Name, connID)

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
			util.LogWarn("[HT-SVR] [%s] [%s] mesh POST decrypt fail: %v", s.Mapping.Name, ch.connID, err)
			w.WriteHeader(410)
			return
		}
	}

	for len(data) >= 3 {
		ft, payload, err := frame.ReadFrame(bytes.NewReader(data))
		if err != nil {
			util.LogWarn("[HT-SVR] [%s] [%s] mesh POST parse fail: %v", s.Mapping.Name, ch.connID, err)
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

// meshDispatchLoop distributes frames from meshOut to waiting GET requests.
// Priority scheduling is handled by the P2P layer's peerWriteLoop, which
// prioritizes control frames before calling transport.Send().
func (s *HTunnelServer) meshDispatchLoop(ch *htChannel) {
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

// dispatchToWaiter sends msg to the next available GET waiter.
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
	if waitTimeStr := r.Header.Get(htHeaderWaitTime); waitTimeStr != "" {
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

	// Create a waiter with msg and done channels
	waiter := &meshWaiter{
		msg:  make(chan meshMsg, 1),
		done: make(chan struct{}),
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

	// Coalesce additional frames that are already available (non-blocking)
	for buf.Len() < maxBatchSize {
		done := false
		select {
		case msg := <-ch.meshOut:
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

func (t meshChannelTransport) Send(frameType byte, payload []byte, isControl bool) error {
	select {
	case t.ch.meshOut <- meshMsg{frameType: frameType, payload: payload}:
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
		case t.ch.meshOut <- meshMsg{frameType: f.Type, payload: f.Payload}:
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
