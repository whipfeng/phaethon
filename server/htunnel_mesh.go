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
		getWaiters: make(chan chan<- meshMsg, 64), // support up to 64 concurrent GETs
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

// meshDispatchLoop distributes frames from meshOut to waiting GET requests.
// This enables concurrent GET support: multiple GET requests can be pending,
// and frames are distributed to them one by one.
func (s *HTunnelServer) meshDispatchLoop(ch *htChannel) {
	for {
		select {
		case msg := <-ch.meshOut:
			// Wait for a GET request to be ready
			select {
			case waiter := <-ch.getWaiters:
				select {
				case waiter <- msg:
				case <-ch.closed:
					return
				}
			case <-ch.closed:
				return
			}
		case <-ch.closed:
			return
		}
	}
}

// meshHandleRead serves a client long-poll GET: register as a waiter, wait up
// to htMeshPollTimeout for a frame from the dispatcher (408 on timeout),
// coalesce further queued frames into one batch (≤ htMeshBatchLimit) and
// return it encrypted.
func (s *HTunnelServer) meshHandleRead(ch *htChannel, w http.ResponseWriter, r *http.Request) {
	// Create a waiter channel for this GET request
	waiter := make(chan meshMsg, 1)

	// Register as a waiter
	select {
	case ch.getWaiters <- waiter:
	case <-time.After(htMeshPollTimeout):
		w.WriteHeader(408)
		return
	case <-ch.closed:
		w.WriteHeader(410)
		return
	}

	// Wait for the first frame from the dispatcher
	var buf bytes.Buffer
	select {
	case msg := <-waiter:
		_ = frame.WriteFrame(&buf, msg.frameType, msg.payload)
	case <-time.After(htMeshPollTimeout):
		w.WriteHeader(408)
		return
	case <-ch.closed:
		w.WriteHeader(410)
		return
	}

	// Coalesce additional frames that are already available (non-blocking)
	for buf.Len() < htMeshBatchLimit {
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

func (t meshChannelTransport) Send(frameType byte, payload []byte) error {
	select {
	case t.ch.meshOut <- meshMsg{frameType: frameType, payload: payload}:
		return nil
	case <-t.ch.closed:
		return io.ErrClosedPipe
	}
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
