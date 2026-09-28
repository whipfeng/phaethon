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

	// Send concurrency control
	sendSem chan struct{} // semaphore for concurrent POST requests

	// Recv: separate channels for control and data frames
	ctrlRecvCh chan recvResult // control frames (heartbeat, hello, gossip, probe)
	dataRecvCh chan recvResult // data frames (FrameMeshPacket)

	seqMu  sync.Mutex // guards writeSeq/deleteSeq

	writeSeq  int
	deleteSeq int

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

	t := &htunnelDirectTransport{
		proxy:        proxy,
		connectionID: connectionID,
		client:       client,
		crypto:       crypto,
		sendSem:      make(chan struct{}, htunnelConcurrency),
		ctrlRecvCh:   make(chan recvResult, 16),
		dataRecvCh:   make(chan recvResult, 64),
		closed:       make(chan struct{}),
	}

	// Start 1 control recvLoop + N data recvLoops
	go t.recvLoop(true)
	for i := 0; i < htunnelConcurrency-1; i++ {
		go t.recvLoop(false)
	}

	return t, nil
}

// Send writes one frame. Both control and data frames use the same async POST
// path with sendSem concurrency control. Priority scheduling is handled by
// the P2P layer's peerWriteLoop, which prioritizes control frames before
// calling transport.Send().
func (t *htunnelDirectTransport) Send(frameType byte, payload []byte, isControl bool) error {
	return t.sendFrame(frameType, payload)
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

		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
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

			reader := bytes.NewReader(body)
			for reader.Len() > 0 {
				ft, payload, err := frame.ReadFrame(reader)
				if err != nil {
					t.sendError(fmt.Errorf("htunnel-direct: parse frame fail: %w", err), isCtrlLoop)
					return
				}
				t.routeFrame(ft, payload, isCtrlLoop)
			}
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
