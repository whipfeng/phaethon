package dialer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"phaethon/config"
	"phaethon/frame"
	"phaethon/util"
)

// HTunnelCmdMesh marks a channel request as a P2P mesh channel: no target
// address semantics, frames flow directly in POST/GET bodies.
const HTunnelCmdMesh = "MESH"

// htunnelConcurrency is the number of concurrent POST and GET requests
// for data frames. This provides high throughput while limiting resource usage.
const htunnelConcurrency = 16

// Ordered-dispatch batch caps: one POST carries at most one of these per
// flow. The remainder stays queued and ships in the flow's next batch
// (after the in-flight one completes), preserving per-flow FIFO.
const (
	maxOrdBatchFrames = 64
	maxOrdBatchBytes  = 256 * 1024
)

// htunnelDirectTransport implements frame.FrameTransport over h_tunnel
// without the BIND stream channel: one HEAD (X-C: MESH) allocates the
// channel, frames travel in POST bodies (client → server) and long-poll GET
// bodies (server → client). Control and data frames use separate receive
// buffers to prevent data congestion from starving control traffic.
type htunnelDirectTransport struct {
	proxy        *config.Proxy
	connectionID string
	client       *http.Client
	crypto       *util.HTunnelCrypto
	htConfig     *config.HTunnelConfig // v0.2.0: h_tunnel config

	// Send concurrency control
	sendSem chan struct{} // semaphore for concurrent POST requests

	// Recv: separate channels for control and data frames
	ctrlRecvCh chan recvResult // control frames (heartbeat, hello, gossip, probe)
	dataRecvCh chan recvResult // data frames (FrameMeshPacket)

	seqMu  sync.Mutex // guards writeSeq/deleteSeq

	writeSeq  int
	deleteSeq int

	// v0.2.0: GET dynamic scaling
	getMu       sync.Mutex
	activeGETs  int           // current number of GET goroutines
	fullCount   int           // consecutive full batch count
	emptyCount  int           // consecutive empty batch count
	waitTime    time.Duration // current wait time (adaptive)
	baseGETs    int           // fixed GET slots (default 2)

	// v0.2.0: heartbeat
	heartbeatStop chan struct{}

	closed    chan struct{}
	closeOnce sync.Once

	// Per-flow ordered send (design mesh-per-flow-ordering §3.1): data
	// frames are queued per inner flow and dispatched so that a flow has
	// at most one POST batch in flight. POST batches travel on a single
	// ordered pipe (meshIn channel) server-side, giving strict per-flow
	// FIFO across the concurrent POST pool. Without this, frames of one
	// flow spread over up to 16 in-flight batches and arrive reordered
	// (~50% OOO observed), collapsing the inner sender's cwnd.
	ordMu       sync.Mutex
	ordCond     *sync.Cond             // signalled when ordBytes drops or on close
	ordPending  map[uint32]*ordFlowBuf // flow key -> queued frames
	ordFIFO     []uint32               // flow keys with queued frames, arrival order
	ordInflight map[uint32]bool        // flow keys with an uncompleted POST
	ordWake     chan struct{}          // enqueue / completion signal
	ordBytes    int                    // payload bytes queued across all flows
	ordClosed   bool
}

// ordPendingCap bounds the ordered send queue (plan §3.9 R3): past it
// enqueueOrdered blocks, pushing backpressure into the p2p write loop.
const ordPendingCap = 4 << 20

// ordFlowBuf holds one flow's queued frames between dispatches.
type ordFlowBuf struct {
	frames []frame.Frame
	bytes  int
}

// recvResult holds a frame received from a GET request, or an error.
type recvResult struct {
	frameType byte
	payload   []byte
	err       error
}

// dialP2PDirect establishes a mesh channel via a single HEAD and returns the
// direct frame transport.
func (d *HTunnelDialer) dialP2PDirect() (frame.FrameTransport, error) {
	proxy := d.Proxy
	util.LogModuleInfo("htunnel", "dialP2PDirect called, URL=%s", proxy.Name, proxy.URL)
	crypto := util.NewHTunnelCrypto(proxy.Password)

	req, _ := http.NewRequest("HEAD", fmt.Sprintf("%s//0", proxy.URL), nil)
	req.Header.Set(HeaderContentSeq, strconv.Itoa(0))
	req.Header.Set(HeaderCommand, HTunnelCmdMesh)

	client := sharedHTunnelClient(proxy)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("htunnel-direct: channel request fail: %w", err)
	}
	resp.Body.Close()
	cancel()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("htunnel-direct: channel request status: %d", resp.StatusCode)
	}
	connectionID := resp.Header.Get(HeaderConnectionID)
	if connectionID == "" {
		return nil, fmt.Errorf("htunnel-direct: no connection ID returned")
	}

	util.LogModuleDebug("htunnel", "mesh channel established via %s (connectionID=%s)", proxy.Name, d.ConnIDStr(), proxy.URL, connectionID)

	// Snapshot per channel: the mode must not change under a live channel.
	htConfig := config.HTunnel()
	baseGETs := htConfig.GetGETSlots()
	_, _, defaultWait := htConfig.GetWaitTimeRange()

	t := &htunnelDirectTransport{
		proxy:        proxy,
		connectionID: connectionID,
		client:       client,
		crypto:       crypto,
		htConfig:     htConfig,
		sendSem:      make(chan struct{}, htConfig.GetPoolSize()),
		ctrlRecvCh:   make(chan recvResult, 16),
		dataRecvCh:   make(chan recvResult, 64),
		closed:       make(chan struct{}),
		// v0.2.0: GET dynamic scaling
		baseGETs:   baseGETs,
		activeGETs: baseGETs,
		waitTime:   defaultWait,
		// v0.2.0: heartbeat
		heartbeatStop: make(chan struct{}),
		// per-flow ordered dispatch
		ordPending:  make(map[uint32]*ordFlowBuf),
		ordInflight: make(map[uint32]bool),
		ordWake:     make(chan struct{}, 1),
	}
	t.ordCond = sync.NewCond(&t.ordMu)

	// Per-flow ordered dispatcher for data frames.
	go t.orderedDispatchLoop()

	// v0.2.0: Start base GET recvLoops (default 2)
	for i := 0; i < baseGETs; i++ {
		go t.recvLoop(i) // pipe index i; first one is the control loop
	}

	// v0.2.0: Start heartbeat goroutine
	go t.heartbeatLoop()

	return t, nil
}

// Send writes one frame. Control frames use the direct async POST path
// (priority, unordered is fine). Data frames go through the per-flow
// ordered dispatcher so each inner flow keeps FIFO across the POST pool.
// Priority scheduling is handled by the P2P layer's peerWriteLoop, which
// prioritizes control frames before calling transport.Send().
func (t *htunnelDirectTransport) Send(frameType byte, payload []byte, isControl bool) error {
	if isControl || t.htConfig.GetAllowFlowReorder() {
		return t.sendFrame(frameType, payload)
	}
	return t.enqueueOrdered([]frame.Frame{{Type: frameType, Payload: payload}})
}

// enqueueOrdered appends frames to their per-flow queues and wakes the
// dispatcher. Per-flow order is preserved because p2p's batching and the
// stripe lane pin already emit a flow's frames in order. Blocks while the
// queue is at ordPendingCap; one call may overshoot the cap by its batch.
func (t *htunnelDirectTransport) enqueueOrdered(frames []frame.Frame) error {
	t.ordMu.Lock()
	for t.ordBytes >= ordPendingCap && !t.ordClosed {
		t.ordCond.Wait()
	}
	if t.ordClosed {
		t.ordMu.Unlock()
		return io.ErrClosedPipe
	}
	for _, f := range frames {
		key := frame.FlowKey(f.Payload)
		buf := t.ordPending[key]
		if buf == nil {
			buf = &ordFlowBuf{}
			t.ordPending[key] = buf
			t.ordFIFO = append(t.ordFIFO, key)
		}
		buf.frames = append(buf.frames, f)
		buf.bytes += len(f.Payload)
		t.ordBytes += len(f.Payload)
	}
	t.ordMu.Unlock()
	select {
	case t.ordWake <- struct{}{}:
	default:
	}
	return nil
}

// orderedDispatchLoop ships per-flow batches: at most one POST batch per
// flow is in flight at any time, so a flow's batches arrive server-side in
// emission order (each POST is applied to the channel's FIFO meshIn before
// its response is sent). Cross-flow parallelism keeps the POST pool busy.
func (t *htunnelDirectTransport) orderedDispatchLoop() {
	for {
		t.ordMu.Lock()
		found := -1
		var key uint32
		for i, k := range t.ordFIFO {
			if t.ordInflight[k] {
				continue
			}
			if buf := t.ordPending[k]; buf != nil && len(buf.frames) > 0 {
				found, key = i, k
				break
			}
		}
		var batch []frame.Frame
		if found >= 0 {
			buf := t.ordPending[key]
			n, sz := 0, 0
			for n < len(buf.frames) && n < maxOrdBatchFrames && sz < maxOrdBatchBytes {
				sz += len(buf.frames[n].Payload)
				n++
			}
			batch = buf.frames[:n]
			buf.frames = buf.frames[n:]
			buf.bytes -= sz
			t.ordBytes -= sz
			t.ordCond.Broadcast()
			t.ordInflight[key] = true
			// Rotate: dispatched flow goes to the FIFO tail when it still
			// has queued frames, otherwise it leaves the rotation.
			t.ordFIFO = append(t.ordFIFO[:found], t.ordFIFO[found+1:]...)
			if len(buf.frames) > 0 {
				t.ordFIFO = append(t.ordFIFO, key)
			} else {
				delete(t.ordPending, key)
			}
		}
		t.ordMu.Unlock()

		if found < 0 {
			select {
			case <-t.closed:
				return
			case <-t.ordWake:
			}
			continue
		}

		var buf bytes.Buffer
		for _, f := range batch {
			if err := frame.WriteFrame(&buf, f.Type, f.Payload); err != nil {
				util.LogModuleWarn("htunnel", "ordered batch serialize fail (flow=%d): %v", key, err)
				break
			}
		}
		data := buf.Bytes()
		if len(data) == 0 {
			t.ordRelease(key)
			continue
		}

		t.seqMu.Lock()
		t.writeSeq++
		seq := t.writeSeq
		t.seqMu.Unlock()

		select {
		case t.sendSem <- struct{}{}:
		case <-t.closed:
			t.ordRelease(key)
			return
		}
		go func(key uint32) {
			defer func() {
				<-t.sendSem
				t.ordMu.Lock()
				delete(t.ordInflight, key)
				t.ordMu.Unlock()
				select {
				case t.ordWake <- struct{}{}:
				default:
				}
			}()
			if err := t.postBatch(data, seq); err != nil {
				util.LogModuleDebug("htunnel", "ordered batch POST fail (seq=%d, frames=%d): %v", seq, len(batch), err)
			}
		}(key)
	}
}

// ordRelease clears a flow's in-flight mark without a completed POST
// (close / serialization error paths).
func (t *htunnelDirectTransport) ordRelease(key uint32) {
	t.ordMu.Lock()
	delete(t.ordInflight, key)
	t.ordMu.Unlock()
	select {
	case t.ordWake <- struct{}{}:
	default:
	}
}

// SendBatch queues data frames for per-flow ordered dispatch: frames are
// grouped by inner flow and each flow ships at most one POST batch at a
// time (see orderedDispatchLoop). With flow reordering allowed it falls
// back to the legacy whole-batch single POST.
func (t *htunnelDirectTransport) SendBatch(frames []frame.Frame) error {
	if len(frames) == 0 {
		return nil
	}
	if t.htConfig.GetAllowFlowReorder() {
		if len(frames) == 1 {
			return t.sendFrame(frames[0].Type, frames[0].Payload)
		}

		select {
		case <-t.closed:
			return io.ErrClosedPipe
		default:
		}

		// Serialize all frames into one buffer
		var buf bytes.Buffer
		for _, f := range frames {
			if err := frame.WriteFrame(&buf, f.Type, f.Payload); err != nil {
				return err
			}
		}
		data := buf.Bytes()

		t.seqMu.Lock()
		t.writeSeq++
		seq := t.writeSeq
		t.seqMu.Unlock()

		// Acquire semaphore (blocks if htunnelConcurrency POSTs are in flight)
		select {
		case t.sendSem <- struct{}{}:
		case <-t.closed:
			return io.ErrClosedPipe
		}

		go func() {
			defer func() { <-t.sendSem }()
			if err := t.postBatch(data, seq); err != nil {
				util.LogModuleDebug("htunnel", "batch POST fail (seq=%d, frames=%d): %v", seq, len(frames), err)
			}
		}()
		return nil
	}
	return t.enqueueOrdered(frames)
}

func (t *htunnelDirectTransport) sendFrame(frameType byte, payload []byte) error {
	select {
	case <-t.closed:
		return io.ErrClosedPipe
	default:
	}

	var buf bytes.Buffer
	if err := frame.WriteFrame(&buf, frameType, payload); err != nil {
		return err
	}
	data := buf.Bytes()

	t.seqMu.Lock()
	t.writeSeq++
	seq := t.writeSeq
	t.seqMu.Unlock()

	// Acquire semaphore (blocks if htunnelConcurrency POSTs are in flight)
	select {
	case t.sendSem <- struct{}{}:
	case <-t.closed:
		return io.ErrClosedPipe
	}

	go func() {
		defer func() { <-t.sendSem }()
		if err := t.postBatch(data, seq); err != nil {
			util.LogModuleDebug("htunnel", "POST fail (seq=%d): %v", seq, err)
		}
	}()
	return nil
}

func (t *htunnelDirectTransport) postBatch(plaintext []byte, seq int) error {
	select {
	case <-t.closed:
		return io.ErrClosedPipe
	default:
	}

	data := plaintext
	if t.crypto.IsEnabled() {
		data = t.crypto.SealBody(plaintext)
	}

	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/%s/%d", t.proxy.URL, t.connectionID, seq), bytes.NewReader(data))
	req.Header.Set(HeaderConnectionID, t.connectionID)
	req.Header.Set(HeaderContentSeq, strconv.Itoa(seq))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	resp, err := t.client.Do(req.WithContext(ctx))
	if err != nil {
		cancel()
		return fmt.Errorf("htunnel-direct: post fail: %w", err)
	}
	resp.Body.Close()
	cancel()

	if resp.StatusCode == 410 {
		return io.ErrClosedPipe
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("htunnel-direct: post status: %d", resp.StatusCode)
	}
	return nil
}

// recvLoop continuously sends GET requests and routes received frames to
// ctrlRecvCh or dataRecvCh based on frame type. Multiple recvLoop goroutines
// run concurrently to eliminate the gap between GETs. Each loop is one
// ordered "pipe": with per-flow ordering enabled the loop index is sent
// to the server (X-Q/X-N) so frames of one inner flow always ride the
// same serial GET chain.
func (t *htunnelDirectTransport) recvLoop(pipeIdx int) {
	isCtrlLoop := pipeIdx == 0
	loopType := "data"
	if isCtrlLoop {
		loopType = "ctrl"
	}
	util.LogModuleDebug("htunnel", "recvLoop started: proxy=%s connID=%s type=%s", t.proxy.Name, t.connectionID, loopType)
	readSeq := 0
	for {
		select {
		case <-t.closed:
			util.LogModuleDebug("htunnel", "recvLoop exiting: proxy=%s connID=%s type=%s closed", t.proxy.Name, t.connectionID, loopType)
			return
		default:
		}

		readSeq++
		seq := readSeq
		getURL := fmt.Sprintf("%s/%s/%d", t.proxy.URL, t.connectionID, seq)
		req, _ := http.NewRequest("GET", getURL, nil)
		req.Header.Set(HeaderConnectionID, t.connectionID)
		req.Header.Set(HeaderContentSeq, strconv.Itoa(seq))
		if !t.htConfig.GetAllowFlowReorder() {
			req.Header.Set(HeaderPipeIndex, strconv.Itoa(pipeIdx))
			req.Header.Set(HeaderPipeCount, strconv.Itoa(t.baseGETs))
		}

		// v0.2.0: send client-controlled wait time
		t.getMu.Lock()
		waitTime := t.waitTime
		t.getMu.Unlock()
		req.Header.Set(HeaderWaitTime, strconv.Itoa(int(waitTime.Seconds())))

		// Context timeout = wait time + buffer for network latency
		ctxTimeout := waitTime + 10*time.Second
		ctx, cancel := context.WithTimeout(context.Background(), ctxTimeout)
		util.LogModuleDebug("htunnel", "sending GET: proxy=%s connID=%s seq=%d url=%s", t.proxy.Name, t.connectionID, seq, getURL)
		resp, err := t.client.Do(req.WithContext(ctx))
		if err != nil {
			cancel()
			util.LogModuleWarn("htunnel", "GET failed: proxy=%s connID=%s seq=%d err=%v", t.proxy.Name, t.connectionID, seq, err)
			t.sendError(fmt.Errorf("htunnel-direct: get fail: %w", err), isCtrlLoop)
			return
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()

		util.LogModuleDebug("htunnel", "GET response: proxy=%s connID=%s seq=%d status=%d bodyLen=%d", t.proxy.Name, t.connectionID, seq, resp.StatusCode, len(body))

		if resp.StatusCode == 410 {
			util.LogModuleWarn("htunnel", "GET 410: proxy=%s connID=%s seq=%d server closed channel", t.proxy.Name, t.connectionID, seq)
			t.sendError(io.ErrClosedPipe, isCtrlLoop)
			return
		}
		if resp.StatusCode == 408 {
			continue // timeout, retry
		}
		if resp.StatusCode != 200 {
			util.LogModuleWarn("htunnel", "GET bad status: proxy=%s connID=%s seq=%d status=%d", t.proxy.Name, t.connectionID, seq, resp.StatusCode)
			t.sendError(fmt.Errorf("htunnel-direct: get status: %d", resp.StatusCode), isCtrlLoop)
			return
		}

		if len(body) > 0 {
			if t.crypto.IsEnabled() {
				body, err = t.crypto.OpenBody(body)
				if err != nil {
					util.LogModuleWarn("htunnel", "decrypt failed: proxy=%s connID=%s seq=%d err=%v", t.proxy.Name, t.connectionID, seq, err)
					t.sendError(fmt.Errorf("htunnel-direct: decrypt fail: %w", err), isCtrlLoop)
					return
				}
			}

			// v0.2.0: track batch size for dynamic scaling
			maxBatchSize := t.htConfig.GetMaxBatchSize()
			isFullBatch := len(body) >= maxBatchSize
			t.adjustWaitTime(isFullBatch)

			reader := bytes.NewReader(body)
			frameCount := 0
			for reader.Len() > 0 {
				ft, payload, err := frame.ReadFrame(reader)
				if err != nil {
					util.LogModuleWarn("htunnel", "frame parse failed: proxy=%s connID=%s seq=%d frames=%d err=%v", t.proxy.Name, t.connectionID, seq, frameCount, err)
					t.sendError(fmt.Errorf("htunnel-direct: parse frame fail: %w", err), isCtrlLoop)
					return
				}
				frameCount++
				util.LogModuleDebug("htunnel", "frame received: proxy=%s connID=%s seq=%d frame=%d type=%d payloadLen=%d", t.proxy.Name, t.connectionID, seq, frameCount, ft, len(payload))
				t.routeFrame(ft, payload, isCtrlLoop)
				// Note: payload ownership transferred to routeFrame -> Recv() -> P2P readLoop
				// P2P readLoop will return the buffer (issue 10)
			}
			util.LogModuleDebug("htunnel", "batch complete: proxy=%s connID=%s seq=%d frames=%d", t.proxy.Name, t.connectionID, seq, frameCount)
		} else {
			// v0.2.0: empty batch
			t.adjustWaitTime(false)
		}
	}
}

// sendError sends an error result to the appropriate receive channel.
func (t *htunnelDirectTransport) sendError(err error, isCtrlLoop bool) {
	ch := t.dataRecvCh
	if isCtrlLoop {
		ch = t.ctrlRecvCh
	}
	select {
	case ch <- recvResult{err: err}:
	case <-t.closed:
	}
}

// routeFrame routes a frame to the appropriate receive channel based on type.
func (t *htunnelDirectTransport) routeFrame(ft byte, payload []byte, isCtrlLoop bool) {
	isData := ft == frame.FrameMeshPacket
	ch := t.dataRecvCh
	if !isData {
		ch = t.ctrlRecvCh
	}
	select {
	case ch <- recvResult{frameType: ft, payload: payload}:
	case <-t.closed:
	}
}

// Recv returns the next frame from the server. Control frames are prioritized
// over data frames to prevent data congestion from delaying control traffic.
func (t *htunnelDirectTransport) Recv() (byte, []byte, error) {
	// Priority: non-blocking check for control frames
	select {
	case <-t.closed:
		return 0, nil, io.ErrClosedPipe
	case result := <-t.ctrlRecvCh:
		if result.err != nil {
			return 0, nil, result.err
		}
		return result.frameType, result.payload, nil
	default:
	}

	// Block on either channel
	select {
	case <-t.closed:
		return 0, nil, io.ErrClosedPipe
	case result := <-t.ctrlRecvCh:
		if result.err != nil {
			return 0, nil, result.err
		}
		return result.frameType, result.payload, nil
	case result := <-t.dataRecvCh:
		if result.err != nil {
			return 0, nil, result.err
		}
		return result.frameType, result.payload, nil
	}
}

// Close closes the transport and DELETEs the server-side channel (best effort).
// Upper layers reconnect on session end.
func (t *htunnelDirectTransport) Close() error {
	t.closeOnce.Do(func() {
		close(t.closed)
		t.ordMu.Lock()
		t.ordClosed = true
		t.ordCond.Broadcast()
		t.ordMu.Unlock()

		// v0.2.0: stop heartbeat goroutine
		select {
		case <-t.heartbeatStop:
		default:
			close(t.heartbeatStop)
		}

		t.seqMu.Lock()
		t.deleteSeq++
		seq := t.deleteSeq
		t.seqMu.Unlock()

		req, _ := http.NewRequest("DELETE", fmt.Sprintf("%s/%s/%d", t.proxy.URL, t.connectionID, seq), nil)
		req.Header.Set(HeaderConnectionID, t.connectionID)
		req.Header.Set(HeaderContentSeq, strconv.Itoa(seq))

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		resp, err := t.client.Do(req.WithContext(ctx))
		if err == nil {
			resp.Body.Close()
		}
		cancel()
	})
	return nil
}

// adjustWaitTime adapts the GET wait time based on batch fullness.
// Full batch → decrease wait time (high traffic, poll faster).
// Empty batch → increase wait time (low traffic, poll slower).
func (t *htunnelDirectTransport) adjustWaitTime(isFullBatch bool) {
	t.getMu.Lock()
	defer t.getMu.Unlock()

	minWait, maxWait, _ := t.htConfig.GetWaitTimeRange()

	// Safe connection ID for logging (handle short IDs)
	connIDLog := t.connectionID
	if len(connIDLog) > 8 {
		connIDLog = connIDLog[:8]
	}

	if isFullBatch {
		t.fullCount++
		t.emptyCount = 0
		// 3 consecutive full batches → decrease wait time
		if t.fullCount >= 3 {
			newWait := t.waitTime / 2
			if newWait < minWait {
				newWait = minWait
			}
			if newWait != t.waitTime {
				util.LogModuleDebug("htunnel", "wait time decreased: %v → %v (full batches)",
					connIDLog, t.waitTime, newWait)
				t.waitTime = newWait
			}
			t.fullCount = 0
		}
	} else {
		t.emptyCount++
		t.fullCount = 0
		// 10 consecutive empty batches → increase wait time
		if t.emptyCount >= 10 {
			newWait := t.waitTime * 2
			if newWait > maxWait {
				newWait = maxWait
			}
			if newWait != t.waitTime {
				util.LogModuleDebug("htunnel", "wait time increased: %v → %v (empty batches)",
					connIDLog, t.waitTime, newWait)
				t.waitTime = newWait
			}
			t.emptyCount = 0
		}
	}
}

// heartbeatLoop sends periodic PUT heartbeats to keep the channel alive.
func (t *htunnelDirectTransport) heartbeatLoop() {
	interval := t.htConfig.GetHeartbeatInterval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-t.closed:
			return
		case <-t.heartbeatStop:
			return
		case <-ticker.C:
			t.sendHeartbeat()
		}
	}
}

// sendHeartbeat sends a single PUT request to keep the channel alive.
func (t *htunnelDirectTransport) sendHeartbeat() {
	select {
	case <-t.closed:
		return
	default:
	}

	t.seqMu.Lock()
	t.writeSeq++
	seq := t.writeSeq
	t.seqMu.Unlock()

	req, _ := http.NewRequest("PUT", fmt.Sprintf("%s/%s/%d", t.proxy.URL, t.connectionID, seq), nil)
	req.Header.Set(HeaderConnectionID, t.connectionID)
	req.Header.Set(HeaderContentSeq, strconv.Itoa(seq))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	resp, err := t.client.Do(req.WithContext(ctx))
	if err != nil {
		cancel()
		// Safe connection ID for logging
		connIDLog := t.connectionID
		if len(connIDLog) > 8 {
			connIDLog = connIDLog[:8]
		}
		util.LogModuleDebug("htunnel", "heartbeat fail (seq=%d): %v", connIDLog, seq, err)
		return
	}
	resp.Body.Close()
	cancel()

	if resp.StatusCode == 410 {
		// Channel gone, trigger reconnect
		t.sendError(io.ErrClosedPipe, false)
	}
}
