package dialer

import (
	"bytes"
	"context"
	"encoding/binary"
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

const (
	// htunnelDirectBatchLimit caps one data-lane POST batch below nginx's
	// default client_max_body_size (1m).
	htunnelDirectBatchLimit = 512 * 1024
	// htunnelDirectPendingMax is the data-lane pending high-water; Send
	// blocks above it (backpressure) until the in-flight batch completes.
	htunnelDirectPendingMax = 1024 * 1024
)

// htunnelDirectTransport implements frame.FrameTransport over h_tunnel
// without the BIND stream channel: one HEAD (X-C: MESH) allocates the
// channel, frames travel in POST bodies (client → server) and long-poll GET
// bodies (server → client). Datagram semantics: the two concurrent lanes may
// drop/duplicate/reorder frames — all P2P frame consumers tolerate this.
type htunnelDirectTransport struct {
	proxy        *config.Proxy
	connectionID string
	client       *http.Client
	crypto       *util.HTunnelCrypto

	// Data lane batching (pendMu guards; in-flight collection: while a POST
	// is in flight new frames append to pending, and the next batch is sent
	// as soon as the POST returns — no timer).
	pendMu   sync.Mutex
	pendCond *sync.Cond
	pending  []byte
	sending  bool
	writeErr error

	ctrlMu sync.Mutex // serializes control-lane POSTs
	seqMu  sync.Mutex // guards writeSeq/deleteSeq

	readMu     sync.Mutex
	readBuf    []byte
	readOffset int
	readSeq    int
	writeSeq   int
	deleteSeq  int

	closed    chan struct{}
	closeOnce sync.Once
}

// dialP2PDirect establishes a mesh channel via a single HEAD and returns the
// direct frame transport.
func (d *HTunnelDialer) dialP2PDirect() (frame.FrameTransport, error) {
	proxy := d.Proxy
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
		closed:       make(chan struct{}),
	}
	t.pendCond = sync.NewCond(&t.pendMu)
	return t, nil
}

// Send writes one frame. Mesh data goes through the batching data lane;
// control frames (heartbeat/hello/gossip) get their own immediate POST so
// they never queue behind bulk data.
func (t *htunnelDirectTransport) Send(frameType byte, payload []byte) error {
	if frameType == frame.FrameMeshPacket {
		return t.sendData(frameType, payload)
	}
	return t.sendControl(frameType, payload)
}

func (t *htunnelDirectTransport) sendControl(frameType byte, payload []byte) error {
	select {
	case <-t.closed:
		return io.ErrClosedPipe
	default:
	}
	t.ctrlMu.Lock()
	defer t.ctrlMu.Unlock()

	select {
	case <-t.closed:
		return io.ErrClosedPipe
	default:
	}

	var buf bytes.Buffer
	if err := frame.WriteFrame(&buf, frameType, payload); err != nil {
		return err
	}
	return t.postBatch(buf.Bytes())
}

func (t *htunnelDirectTransport) sendData(frameType byte, payload []byte) error {
	t.pendMu.Lock()
	// Backpressure: block while pending is above the high-water mark.
	for len(t.pending) >= htunnelDirectPendingMax {
		select {
		case <-t.closed:
			t.pendMu.Unlock()
			return io.ErrClosedPipe
		default:
		}
		t.pendCond.Wait()
	}
	if t.writeErr != nil {
		err := t.writeErr
		t.pendMu.Unlock()
		return err
	}
	var buf bytes.Buffer
	if err := frame.WriteFrame(&buf, frameType, payload); err != nil {
		t.pendMu.Unlock()
		return err
	}
	t.pending = append(t.pending, buf.Bytes()...)
	flush := !t.sending
	if flush {
		t.sending = true
	}
	t.pendMu.Unlock()

	if flush {
		go t.flushLoop()
	}
	return nil
}

// flushLoop drains pending in bounded batches while frames keep arriving.
// It holds the sending claim until pending is empty, so at most one data
// POST is in flight. A POST failure records writeErr (surfaced by later
// Send/Recv, ending the P2P session) and drops unsent frames.
func (t *htunnelDirectTransport) flushLoop() {
	for {
		t.pendMu.Lock()
		batch := t.takeBatchLocked()
		if len(batch) == 0 {
			t.sending = false
			t.pendCond.Broadcast()
			t.pendMu.Unlock()
			return
		}
		t.pendMu.Unlock()

		err := t.postBatch(batch)

		t.pendMu.Lock()
		if err != nil {
			t.writeErr = err
			t.pending = nil
			t.sending = false
			t.pendCond.Broadcast()
			t.pendMu.Unlock()
			return
		}
		t.pendCond.Broadcast() // pending shrank; wake backpressure waiters
		t.pendMu.Unlock()
	}
}

// takeBatchLocked removes up to htunnelDirectBatchLimit of pending frames
// (cut at a frame boundary) and returns the plaintext batch.
func (t *htunnelDirectTransport) takeBatchLocked() []byte {
	if len(t.pending) == 0 {
		return nil
	}
	n := len(t.pending)
	if n > htunnelDirectBatchLimit {
		n = cutBatchAtFrame(t.pending, htunnelDirectBatchLimit)
	}
	batch := make([]byte, n)
	copy(batch, t.pending)
	rest := copy(t.pending, t.pending[n:])
	t.pending = t.pending[:rest]
	return batch
}

// cutBatchAtFrame returns the largest prefix ≤ limit ending on a frame
// boundary (3-byte header: type + big-endian uint16 length).
func cutBatchAtFrame(buf []byte, limit int) int {
	pos := 0
	for pos+3 <= len(buf) {
		frameLen := 3 + int(binary.BigEndian.Uint16(buf[pos+1:pos+3]))
		if pos+frameLen > limit {
			break
		}
		pos += frameLen
	}
	if pos == 0 && len(buf) >= 3 {
		// A single frame can't exceed the limit (frames are ≤ 65538 bytes);
		// take the first frame regardless to make progress.
		pos = 3 + int(binary.BigEndian.Uint16(buf[1:3]))
	}
	return pos
}

func (t *htunnelDirectTransport) postBatch(plaintext []byte) error {
	select {
	case <-t.closed:
		return io.ErrClosedPipe
	default:
	}

	data := plaintext
	if t.crypto.IsEnabled() {
		data = t.crypto.SealBody(plaintext)
	}

	t.seqMu.Lock()
	t.writeSeq++
	seq := t.writeSeq
	t.seqMu.Unlock()

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

// Recv blocks for the next frame from the server, cycling long-poll GETs
// (40s context; 408 = server-side poll timeout, retry). Liveness: any GET
// failure ends the session; no htunnel-layer heartbeat needed.
func (t *htunnelDirectTransport) Recv() (byte, []byte, error) {
	t.readMu.Lock()
	defer t.readMu.Unlock()

	for {
		select {
		case <-t.closed:
			return 0, nil, io.ErrClosedPipe
		default:
		}
		t.pendMu.Lock()
		err := t.writeErr
		t.pendMu.Unlock()
		if err != nil {
			return 0, nil, err
		}

		if t.readOffset < len(t.readBuf) {
			ft, payload, err := frame.ReadFrame(bytes.NewReader(t.readBuf[t.readOffset:]))
			if err != nil {
				return 0, nil, fmt.Errorf("htunnel-direct: parse batch fail: %w", err)
			}
			t.readOffset += 3 + len(payload)
			return ft, payload, nil
		}

		t.readSeq++
		seq := t.readSeq
		req, _ := http.NewRequest("GET", fmt.Sprintf("%s/%s/%d", t.proxy.URL, t.connectionID, seq), nil)
		req.Header.Set(headerConnectionID, t.connectionID)
		req.Header.Set(headerContentSeq, strconv.Itoa(seq))

		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		resp, err := t.client.Do(req.WithContext(ctx))
		if err != nil {
			cancel()
			return 0, nil, fmt.Errorf("htunnel-direct: get fail: %w", err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()

		if resp.StatusCode == 410 {
			return 0, nil, io.ErrClosedPipe
		}
		if resp.StatusCode == 408 {
			continue
		}
		if resp.StatusCode != 200 {
			return 0, nil, fmt.Errorf("htunnel-direct: get status: %d", resp.StatusCode)
		}
		if len(body) > 0 && t.crypto.IsEnabled() {
			body, err = t.crypto.OpenBody(body)
			if err != nil {
				return 0, nil, fmt.Errorf("htunnel-direct: decrypt fail: %w", err)
			}
		}
		t.readBuf = body
		t.readOffset = 0
	}
}

// Close closes the transport, drops unsent pending frames and DELETEs the
// server-side channel (best effort). Upper layers reconnect on session end.
func (t *htunnelDirectTransport) Close() error {
	t.closeOnce.Do(func() {
		close(t.closed)

		t.pendMu.Lock()
		t.pending = nil
		t.pendCond.Broadcast()
		t.pendMu.Unlock()

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
