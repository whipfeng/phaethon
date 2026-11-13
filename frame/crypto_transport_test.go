package frame

import (
	"bytes"
	"errors"
	"sync"
	"testing"
)

// memTransport is a minimal in-memory FrameTransport: Send appends to a queue,
// Recv pops from it. Used to exercise the crypto decorator without sockets.
type memTransport struct {
	mu     sync.Mutex
	q      []Frame
	closed bool
}

func (t *memTransport) Send(ft byte, payload []byte, isControl bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	cp := append([]byte(nil), payload...)
	t.q = append(t.q, Frame{Type: ft, Payload: cp, Control: isControl})
	return nil
}

func (t *memTransport) SendBatch(frames []Frame) error {
	for _, f := range frames {
		if err := t.Send(f.Type, f.Payload, f.Control); err != nil {
			return err
		}
	}
	return nil
}

func (t *memTransport) Recv() (byte, []byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.q) == 0 {
		return 0, nil, errors.New("empty")
	}
	f := t.q[0]
	t.q = t.q[1:]
	return f.Type, f.Payload, nil
}

func (t *memTransport) Close() error { return nil }

func TestCryptoTransportRoundTrip(t *testing.T) {
	base := &memTransport{}
	ct := NewCryptoTransport(base, "mesh-psk").(*cryptoTransport)
	if err := ct.Send(FrameMeshPacket, []byte("hello-world"), false); err != nil {
		t.Fatalf("Send: %v", err)
	}
	base.mu.Lock()
	if len(base.q) != 1 {
		base.mu.Unlock()
		t.Fatalf("base queue len = %d, want 1", len(base.q))
	}
	sealed := base.q[0].Payload
	base.mu.Unlock()

	if bytes.Equal(sealed, []byte("hello-world")) {
		t.Fatal("payload was not encrypted")
	}
	if len(sealed) != len("hello-world")+40 {
		t.Fatalf("sealed len = %d, want %d (24B nonce + 16B tag)", len(sealed), len("hello-world")+40)
	}

	// Feed the sealed frame back through a transport and decrypt.
	other := &memTransport{q: []Frame{{Type: FrameMeshPacket, Payload: sealed}}}
	recv := NewCryptoTransport(other, "mesh-psk")
	ft, payload, err := recv.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if ft != FrameMeshPacket || !bytes.Equal(payload, []byte("hello-world")) {
		t.Fatalf("round trip = (%#x, %q), want (0x20, hello-world)", ft, payload)
	}
}

func TestCryptoTransportWrongKeyFailsClosed(t *testing.T) {
	base := &memTransport{}
	send := NewCryptoTransport(base, "key-a").(*cryptoTransport)
	if err := send.Send(FrameGossip, []byte("secret"), true); err != nil {
		t.Fatalf("Send: %v", err)
	}
	other := &memTransport{q: base.q}
	recv := NewCryptoTransport(other, "key-b")
	if _, _, err := recv.Recv(); err == nil {
		t.Fatal("Recv with wrong key must fail")
	}
}

func TestCryptoTransportEmptyKeyIsPassthrough(t *testing.T) {
	base := &memTransport{}
	if got := NewCryptoTransport(base, ""); got != FrameTransport(base) {
		t.Fatalf("empty key should return the base transport unchanged, got %T", got)
	}
}

func TestCryptoTransportSendBatchPreservesTypesAndOrder(t *testing.T) {
	base := &memTransport{}
	ct := NewCryptoTransport(base, "mesh-psk")
	in := []Frame{
		{Type: FrameGossip, Payload: []byte("g"), Control: true},
		{Type: FrameMeshPacket, Payload: []byte("m1"), Control: false},
		{Type: FrameMeshPacket, Payload: []byte("m2"), Control: false},
	}
	if err := ct.SendBatch(in); err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	base.mu.Lock()
	defer base.mu.Unlock()
	if len(base.q) != len(in) {
		t.Fatalf("sent %d frames, want %d", len(base.q), len(in))
	}
	for i := range in {
		if base.q[i].Type != in[i].Type || base.q[i].Control != in[i].Control {
			t.Fatalf("frame %d type/control changed: %+v vs %+v", i, base.q[i], in[i])
		}
		if bytes.Equal(base.q[i].Payload, in[i].Payload) {
			t.Fatalf("frame %d payload was not sealed", i)
		}
	}
}
