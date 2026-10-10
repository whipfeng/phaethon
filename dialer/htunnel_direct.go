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
	util.LogInfo("[HTUNNEL-DIRECT] [%s] dialP2PDirect called, URL=%s", proxy.Name, proxy.URL)
	crypto := util.NewHTunnelCrypto(proxy.Password)

	req, _ := http.NewRequest("HEAD", fmt.Sprintf("%s//0", proxy.URL), nil)
	req.Header.Set(headerContentSeq, strconv.Itoa(0))
	req.Header.Set(headerCommand, HTunnelCmdMesh)

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
	connectionID := resp.Header.Get(headerConnectionID)
	if connectionID == "" {
		return nil, fmt.Errorf("htunnel-direct: no connection ID returned")
	}

	util.LogDebug("[HTUNNEL-DIRECT] [%s] [%s] mesh channel established via %s (connectionID=%s)", proxy.Name, d.ConnIDStr(), proxy.URL, connectionID)

	// v0.2.0: get h_tunnel config (nil = use defaults)
	// TODO: pass config through dialer chain instead of accessing global
	var htConfig *config.HTunnelConfig
	// For now, use nil which will trigger default values in Get* methods

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
	}

	// v0.2.0: Start base GET recvLoops (default 2)
	for i := 0; i < baseGETs; i++ {
		go t.recvLoop(i == 0) // first one is control loop
	}

	// v0.2.0: Start heartbeat goroutine
	go t.heartbeatLoop()

	return t, nil
}

// Send writes one frame. Both control and data frames use the same async POST
// path with sendSem concurrency control. Priority scheduling is handled by
// the P2P layer's peerWriteLoop, which prioritizes control frames before
// calling transport.Send().
func (t *htunnelDirectTransport) Send(frameType byte, payload []byte, isControl bool) error {
	return t.sendFrame(frameType, payload)
}

// SendBatch writes multiple frames as a single HTTP POST.
// All frames are serialized into one batch and sent together.
func (t *htunnelDirectTransport) SendBatch(frames []frame.Frame) error {
	if len(frames) == 0 {
		return nil
	}
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
			util.LogDebug("[HTUNNEL-DIRECT] batch POST fail (seq=%d, frames=%d): %v", seq, len(frames), err)
		}
	}()
	return nil
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
			util.LogDebug("[HTUNNEL-DIRECT] POST fail (seq=%d): %v", seq, err)
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
	req.Header.Set(headerConnectionID, t.connectionID)
	req.Header.Set(headerContentSeq, strconv.Itoa(seq))

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
// run concurrently to eliminate the gap between GETs.
func (t *htunnelDirectTransport) recvLoop(isCtrlLoop bool) {
	readSeq := 0
	for {
		select {
		case <-t.closed:
			return
		default:
		}

		readSeq++
		seq := readSeq
		req, _ := http.NewRequest("GET", fmt.Sprintf("%s/%s/%d", t.proxy.URL, t.connectionID, seq), nil)
		req.Header.Set(headerConnectionID, t.connectionID)
		req.Header.Set(headerContentSeq, strconv.Itoa(seq))

		// v0.2.0: send client-controlled wait time
		t.getMu.Lock()
		waitTime := t.waitTime
		t.getMu.Unlock()
		req.Header.Set(headerWaitTime, strconv.Itoa(int(waitTime.Seconds())))

		// Context timeout = wait time + buffer for network latency
		ctxTimeout := waitTime + 10*time.Second
		ctx, cancel := context.WithTimeout(context.Background(), ctxTimeout)
		resp, err := t.client.Do(req.WithContext(ctx))
		if err != nil {
			cancel()
			t.sendError(fmt.Errorf("htunnel-direct: get fail: %w", err), isCtrlLoop)
			return
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()

		if resp.StatusCode == 410 {
			t.sendError(io.ErrClosedPipe, isCtrlLoop)
			return
		}
		if resp.StatusCode == 408 {
			continue // timeout, retry
		}
		if resp.StatusCode != 200 {
			t.sendError(fmt.Errorf("htunnel-direct: get status: %d", resp.StatusCode), isCtrlLoop)
			return
		}

		if len(body) > 0 {
			if t.crypto.IsEnabled() {
				body, err = t.crypto.OpenBody(body)
				if err != nil {
					t.sendError(fmt.Errorf("htunnel-direct: decrypt fail: %w", err), isCtrlLoop)
					return
				}
			}

			// v0.2.0: track batch size for dynamic scaling
			maxBatchSize := t.htConfig.GetMaxBatchSize()
			isFullBatch := len(body) >= maxBatchSize
			t.adjustWaitTime(isFullBatch)

			reader := bytes.NewReader(body)
			for reader.Len() > 0 {
				ft, payload, err := frame.ReadFrame(reader)
				if err != nil {
					t.sendError(fmt.Errorf("htunnel-direct: parse frame fail: %w", err), isCtrlLoop)
					return
				}
				t.routeFrame(ft, payload, isCtrlLoop)
			}
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
		req.Header.Set(headerConnectionID, t.connectionID)
		req.Header.Set(headerContentSeq, strconv.Itoa(seq))

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
				util.LogDebug("[HTUNNEL-DIRECT] [%s] wait time decreased: %v → %v (full batches)",
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
				util.LogDebug("[HTUNNEL-DIRECT] [%s] wait time increased: %v → %v (empty batches)",
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
	req.Header.Set(headerConnectionID, t.connectionID)
	req.Header.Set(headerContentSeq, strconv.Itoa(seq))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	resp, err := t.client.Do(req.WithContext(ctx))
	if err != nil {
		cancel()
		// Safe connection ID for logging
		connIDLog := t.connectionID
		if len(connIDLog) > 8 {
			connIDLog = connIDLog[:8]
		}
		util.LogDebug("[HTUNNEL-DIRECT] [%s] heartbeat fail (seq=%d): %v", connIDLog, seq, err)
		return
	}
	resp.Body.Close()
	cancel()

	if resp.StatusCode == 410 {
		// Channel gone, trigger reconnect
		t.sendError(io.ErrClosedPipe, false)
	}
}
