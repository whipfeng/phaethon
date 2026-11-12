package dialer

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"phaethon/config"
	"phaethon/frame"
	"phaethon/util"
)

// htStreamGet enables the persistent chunked GET carrier (one data stream per
// ht link, length-prefixed batches) instead of N long-poll GET loops. It
// removes the per-batch request/response round trip; control traffic keeps its
// normal long-poll loop. Off by default so it can be A/B tested per node.
var htStreamGet = os.Getenv("PHAETHON_HT_STREAM_GET") == "1"

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
type htunnelDirectTransport struct {	proxy        *config.Proxy
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

	// inbox restores data-batch order when more than one data GET loop is
	// active (get-slots > 2). With a single data loop it is a pass-through
	// because batches arrive in sequence order already.
	inbox *orderedInbox

	// streamUnsupported is set when the server ignores the persistent-stream
	// GET (old server); the data loop then falls back to long-poll.
	streamUnsupported atomic.Bool
	streamLogged      atomic.Bool

	// Diagnostics: per-batch HTTP timing/size aggregates, logged every 5s
	// when the htunnel module is at info or debug level.
	getCount  atomic.Uint64
	getBytes  atomic.Uint64
	getNanos  atomic.Uint64
	getMaxNs  atomic.Uint64
	getSlow   atomic.Uint64
	postCount atomic.Uint64
	postBytes atomic.Uint64
	postNanos atomic.Uint64
	postMaxNs atomic.Uint64
	postSlow  atomic.Uint64

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
	util.LogModuleInfo("htunnel", "dialP2PDirect called, URL=%s", proxy.Name, proxy.URL)
	crypto := util.NewHTunnelCrypto(proxy.Password)

	// Optional WebSocket carrier: one full-duplex connection instead of
	// GET long-polls + POSTs. Opt-in per proxy via PHAETHON_HT_WS. On a dial
	// failure (e.g. a proxy that does not forward Upgrade) fall back to the
	// original carrier instead of dropping the link.
	if wsEnabledFor(proxy.Name) {
		util.LogModuleInfo("htunnel", "carrier=websocket proxy=%s", proxy.Name)
		if tr, err := dialP2PWebSocket(proxy, crypto); err == nil {
			return tr, nil
		} else {
			util.LogModuleWarn("htunnel", "websocket carrier failed, falling back to long-poll: proxy=%s err=%v", proxy.Name, err)
		}
	}

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
	}

	// v0.2.0: Start GET recvLoops. With the persistent stream carrier one
	// length-prefixed data GET replaces the N long-poll data loops; the
	// control loop stays a normal long-poll.
	t.inbox = newOrderedInbox(func(b orderedBatch) {
		t.processBatchBody(b.body, b.loopType, b.readSeq)
	}, t.closed)
	loops := baseGETs
	if htStreamGet && loops < 2 {
		loops = 2
	}
	if htStreamGet && loops > 2 {
		loops = 2
	}
	t.baseGETs = loops
	t.activeGETs = loops
	for i := 0; i < loops; i++ {
		go t.recvLoop(i == 0) // first one is the control loop
	}

	// v0.2.0: Start heartbeat goroutine
	go t.heartbeatLoop()

	// Per-batch HTTP diagnostics (logs only while htunnel is at debug level).
	go t.statsLoop()

	return t, nil
}

// IsStreamCarrier reports false: the h_tunnel POST/GET carrier is a pool of
// independent HTTP requests, not an ordered byte stream, so stripe lanes do
// not apply. WebSocket h_tunnel uses frame.WSTransport, which reports true.
func (t *htunnelDirectTransport) IsStreamCarrier() bool { return false }

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
			util.LogModuleDebug("htunnel", "batch POST fail (seq=%d, frames=%d): %v", seq, len(frames), err)
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
	postStart := time.Now()
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
	t.postCount.Add(1)
	t.postBytes.Add(uint64(len(data)))
	d := time.Since(postStart)
	t.postNanos.Add(uint64(d))
	bumpMax(&t.postMaxNs, uint64(d))
	if d > time.Second {
		t.postSlow.Add(1)
	}
	return nil
}

// bumpMax updates *p to v when v is larger (lock-free max).
func bumpMax(p *atomic.Uint64, v uint64) {
	for {
		cur := p.Load()
		if v <= cur {
			return
		}
		if p.CompareAndSwap(cur, v) {
			return
		}
	}
}

// recvLoop continuously sends GET requests and routes received frames to
// ctrlRecvCh or dataRecvCh based on frame type. Multiple recvLoop goroutines
// run concurrently to eliminate the gap between GETs.
func (t *htunnelDirectTransport) recvLoop(isCtrlLoop bool) {
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
		stream := htStreamGet && !isCtrlLoop && !t.streamUnsupported.Load()
		getURL := fmt.Sprintf("%s/%s/%d", t.proxy.URL, t.connectionID, seq)
		req, _ := http.NewRequest("GET", getURL, nil)
		req.Header.Set(HeaderConnectionID, t.connectionID)
		req.Header.Set(HeaderContentSeq, strconv.Itoa(seq))
		if stream {
			req.Header.Set(HeaderStream, "1")
		}

		// v0.2.0: send client-controlled wait time
		t.getMu.Lock()
		waitTime := t.waitTime
		t.getMu.Unlock()
		req.Header.Set(HeaderWaitTime, strconv.Itoa(int(waitTime.Seconds())))

		// The persistent stream lives until the transport closes; the
		// long-poll GET is bounded by wait time + network latency.
		var ctx context.Context
		var cancel context.CancelFunc
		if stream {
			ctx, cancel = context.WithCancel(context.Background())
			go func() {
				select {
				case <-t.closed:
					cancel()
				case <-ctx.Done():
				}
			}()
		} else {
			ctxTimeout := waitTime + 10*time.Second
			ctx, cancel = context.WithTimeout(context.Background(), ctxTimeout)
		}
		util.LogModuleDebug("htunnel", "sending GET: proxy=%s connID=%s seq=%d url=%s", t.proxy.Name, t.connectionID, seq, getURL)
		getStart := time.Now()
		resp, err := t.client.Do(req.WithContext(ctx))
		if err != nil {
			cancel()
			util.LogModuleWarn("htunnel", "GET failed: proxy=%s connID=%s seq=%d err=%v", t.proxy.Name, t.connectionID, seq, err)
			t.sendError(fmt.Errorf("htunnel-direct: get fail: %w", err), isCtrlLoop)
			return
		}

		// Persistent chunked stream: frames arrive as [4-byte len][sealed batch]
		// until the connection breaks. The data path is single-stream, so
		// batches stay in production order (no multi-GET reordering).
		if stream && resp.Header.Get(HeaderStream) == "1" {
			if !t.streamLogged.Swap(true) {
				// Error level so the switch is visible even when the htunnel
				// module is filtered to error (A/B diagnostics).
				util.LogModuleError("htunnel", "[HT-STREAM] persistent GET established: proxy=%s connID=%s", t.proxy.Name, t.connectionID)
			}
			t.getCount.Add(1)
			t.readBatchStream(resp.Body, loopType)
			resp.Body.Close()
			cancel()
			util.LogModuleInfo("htunnel", "stream GET ended, reconnecting: proxy=%s connID=%s", t.proxy.Name, t.connectionID)
			continue
		}
		if stream {
			// Old server: no echo. Fall back to long-poll for this transport.
			t.streamUnsupported.Store(true)
			util.LogModuleInfo("htunnel", "stream GET unsupported by server, falling back: proxy=%s connID=%s", t.proxy.Name, t.connectionID)
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		if resp.StatusCode == http.StatusOK {
			t.getCount.Add(1)
			t.getBytes.Add(uint64(len(body)))
			d := time.Since(getStart)
			t.getNanos.Add(uint64(d))
			bumpMax(&t.getMaxNs, uint64(d))
			if d > time.Second {
				t.getSlow.Add(1)
			}
		}

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

			// Multiple data GET loops may complete out of order. A data-bearing
			// batch carries a server-assigned sequence (X-B); hand it to the
			// per-transport reorder buffer so frames reach the P2P session in
			// the order the server produced them. Control-only batches have no
			// sequence and are delivered immediately.
			if seqStr := resp.Header.Get(HeaderBatchSeq); seqStr != "" {
				if n, perr := strconv.ParseUint(seqStr, 10, 64); perr == nil && n > 0 {
					t.inbox.push(n, orderedBatch{body: body, loopType: loopType, readSeq: seq})
					continue
				}
			}
			t.processBatchBody(body, loopType, seq)
		} else {
			// v0.2.0: empty batch
			t.adjustWaitTime(false)
		}
	}
}

// processBatchBody decodes one GET response body and routes its frames.
// readBatchStream consumes a persistent chunked GET body: each batch is
// [4-byte big-endian length][sealed frame batch]. Returns when the stream
// breaks; the caller reconnects.
func (t *htunnelDirectTransport) readBatchStream(body io.Reader, loopType string) {
	var hdr [4]byte
	seq := 0
	for {
		if _, err := io.ReadFull(body, hdr[:]); err != nil {
			return
		}
		n := binary.BigEndian.Uint32(hdr[:])
		if n == 0 {
			continue
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(body, buf); err != nil {
			return
		}
		if t.crypto.IsEnabled() {
			plain, err := t.crypto.OpenBody(buf)
			if err != nil {
				util.LogModuleWarn("htunnel", "stream decrypt failed: proxy=%s connID=%s err=%v", t.proxy.Name, t.connectionID, err)
				return
			}
			buf = plain
		}
		seq++
		t.getBytes.Add(uint64(n))
		t.processBatchBody(buf, loopType, seq)
		select {
		case <-t.closed:
			return
		default:
		}
	}
}

func (t *htunnelDirectTransport) processBatchBody(body []byte, loopType string, readSeq int) {
	reader := bytes.NewReader(body)
	frameCount := 0
	for reader.Len() > 0 {
		ft, payload, err := frame.ReadFrame(reader)
		if err != nil {
			util.LogModuleWarn("htunnel", "frame parse failed: proxy=%s connID=%s loop=%s seq=%d frames=%d err=%v", t.proxy.Name, t.connectionID, loopType, readSeq, frameCount, err)
			t.sendError(fmt.Errorf("htunnel-direct: parse frame fail: %w", err), false)
			return
		}
		frameCount++
		util.LogModuleDebug("htunnel", "frame received: proxy=%s connID=%s loop=%s seq=%d frame=%d type=%d payloadLen=%d", t.proxy.Name, t.connectionID, loopType, readSeq, frameCount, ft, len(payload))
		t.routeFrame(ft, payload, false)
		// Note: payload ownership transferred to routeFrame -> Recv() -> P2P readLoop
		// P2P readLoop will return the buffer (issue 10)
	}
	util.LogModuleDebug("htunnel", "batch complete: proxy=%s connID=%s loop=%s seq=%d frames=%d", t.proxy.Name, t.connectionID, loopType, readSeq, frameCount)
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

// statsLoop logs aggregate per-batch GET/POST timings and sizes every 5s while
// the htunnel module is at debug level. The counters are swapped so each line
// covers exactly one window.
func (t *htunnelDirectTransport) statsLoop() {
	tk := time.NewTicker(5 * time.Second)
	defer tk.Stop()
	for {
		select {
		case <-t.closed:
			return
		case <-tk.C:
			if util.GetModuleLogLevel("htunnel") > util.LogLevelInfo {
				continue
			}
			gc := t.getCount.Swap(0)
			gb := t.getBytes.Swap(0)
			gn := t.getNanos.Swap(0)
			gmax := t.getMaxNs.Swap(0)
			gslow := t.getSlow.Swap(0)
			pc := t.postCount.Swap(0)
			pb := t.postBytes.Swap(0)
			pn := t.postNanos.Swap(0)
			pmax := t.postMaxNs.Swap(0)
			pslow := t.postSlow.Swap(0)
			if gc == 0 && pc == 0 {
				continue
			}
			var gAvg, gKB, gRate, pAvg, pKB, pRate float64
			if gc > 0 {
				gAvg = float64(gn) / float64(gc) / 1e6
				gKB = float64(gb) / float64(gc) / 1024
				if gn > 0 {
					gRate = float64(gb) * 1e3 / float64(gn)
				}
			}
			if pc > 0 {
				pAvg = float64(pn) / float64(pc) / 1e6
				pKB = float64(pb) / float64(pc) / 1024
				if pn > 0 {
					pRate = float64(pb) * 1e3 / float64(pn)
				}
			}
			util.LogModuleInfo("htunnel", "[HT-STATS] proxy=%s conn=%s GET n=%d avg=%.1fms max=%.0fms slow=%d batch=%.0fKB rate=%.2fMB/s | POST n=%d avg=%.1fms max=%.0fms slow=%d batch=%.0fKB rate=%.2fMB/s",
				t.proxy.Name, t.connectionID, gc, gAvg, float64(gmax)/1e6, gslow, gKB, gRate, pc, pAvg, float64(pmax)/1e6, pslow, pKB, pRate)
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
