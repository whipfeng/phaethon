package dialer

import (
	"bytes"
	"sync"
	"testing"

	"phaethon/frame"
)

// TestCutBatchAtFrame verifies batch cutting respects frame boundaries.
func TestCutBatchAtFrame(t *testing.T) {
	var buf bytes.Buffer
	sizes := []int{10, 100, 1000}
	for _, n := range sizes {
		if err := frame.WriteFrame(&buf, frame.FrameMeshPacket, make([]byte, n)); err != nil {
			t.Fatal(err)
		}
	}
	full := buf.Bytes()

	// Limit >= full length: everything fits.
	if got := cutBatchAtFrame(full, len(full)); got != len(full) {
		t.Errorf("full batch: got %d, want %d", got, len(full))
	}

	// Limit that cuts mid-second-frame: only the first frame fits (3+10).
	limit := 3 + 10 + 3 + 50
	if got := cutBatchAtFrame(full, limit); got != 13 {
		t.Errorf("mid-frame cut: got %d, want 13", got)
	}

	// Empty pending.
	if got := cutBatchAtFrame(nil, htunnelDirectBatchLimit); got != 0 {
		t.Errorf("empty: got %d, want 0", got)
	}
}

// TestHTunnelDirectTransport_BatchRoundTrip verifies batch assembly and
// frame-by-frame parsing round-trips payloads intact.
func TestHTunnelDirectTransport_BatchRoundTrip(t *testing.T) {
	tr := &htunnelDirectTransport{
		closed: make(chan struct{}),
	}
	tr.pendCond = sync.NewCond(&tr.pendMu)

	payloads := [][]byte{[]byte("one"), bytes.Repeat([]byte("x"), 60000), []byte("three")}
	for _, p := range payloads {
		var buf bytes.Buffer
		if err := frame.WriteFrame(&buf, frame.FrameMeshPacket, p); err != nil {
			t.Fatal(err)
		}
		tr.pending = append(tr.pending, buf.Bytes()...)
	}

	batch := tr.takeBatchLocked()
	if len(batch) == 0 {
		t.Fatal("no batch produced")
	}
	rest := batch
	i := 0
	for len(rest) >= 3 {
		ft, payload, err := frame.ReadFrame(bytes.NewReader(rest))
		if err != nil {
			t.Fatalf("parse frame %d: %v", i, err)
		}
		if ft != frame.FrameMeshPacket {
			t.Fatalf("frame %d: type 0x%02x", i, ft)
		}
		if !bytes.Equal(payload, payloads[i]) {
			t.Fatalf("frame %d: payload mismatch (%d bytes)", i, len(payload))
		}
		rest = rest[3+len(payload):]
		i++
	}
	if i != len(payloads) {
		t.Fatalf("parsed %d frames, want %d", i, len(payloads))
	}
	if len(tr.pending) != 0 {
		t.Fatalf("pending not drained: %d bytes", len(tr.pending))
	}
}
