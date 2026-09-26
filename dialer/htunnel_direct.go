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
		closed:       make(chan struct{}),
	}
	return t, nil
}

// Send writes one frame. Mesh data frames are fire-and-forget (async POST,
// no response wait); control frames (heartbeat/hello/gossip) are sent
// synchronously so the caller knows they reached the server.
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

	t.seqMu.Lock()
	t.writeSeq++
	seq := t.writeSeq
	t.seqMu.Unlock()

	return t.postBatch(buf.Bytes(), seq)
}

// sendData sends a data frame (FrameMeshPacket) via fire-and-forget HTTP POST.
// The frame is serialized, a sequence number is assigned, and a goroutine
// performs the POST. The caller does not wait for the response — upper-layer
// TCP retransmission handles any lost packets.
func (t *htunnelDirectTransport) sendData(frameType byte, payload []byte) error {
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

	go func() {
		if err := t.postBatch(data, seq); err != nil {
			util.LogDebug("[HTUNNEL-DIRECT] data POST fail (seq=%d): %v", seq, err)
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
