package frame

import (
	"fmt"

	"phaethon/util"
)

// cryptoTransport is a carrier-agnostic decorator that seals frame payloads
// with XChaCha20-Poly1305 (mesh-wide PSK) while leaving the frame type in the
// clear. The type must stay visible below the p2p layer: the UDP leg routes
// by frame type, control/bulk priority is type-based, and the stripe hashes
// plaintext payloads for lane affinity (this decorator sits below the stripe).
type cryptoTransport struct {
	base FrameTransport
	c    *util.HTunnelCrypto
}

// NewCryptoTransport wraps base so every payload is authenticated-encrypted.
// key is the mesh-wide PSK; an empty key returns base unchanged (opt-out).
func NewCryptoTransport(base FrameTransport, key string) FrameTransport {
	if key == "" {
		return base
	}
	return &cryptoTransport{base: base, c: util.NewHTunnelCrypto("phaethon-p2p-frame-v1:" + key)}
}

func (t *cryptoTransport) Send(frameType byte, payload []byte, isControl bool) error {
	return t.base.Send(frameType, t.c.SealBody(payload), isControl)
}

func (t *cryptoTransport) SendBatch(frames []Frame) error {
	if len(frames) == 0 {
		return nil
	}
	sealed := make([]Frame, len(frames))
	for i, f := range frames {
		f.Payload = t.c.SealBody(f.Payload)
		sealed[i] = f
	}
	return t.base.SendBatch(sealed)
}

func (t *cryptoTransport) Recv() (byte, []byte, error) {
	frameType, payload, err := t.base.Recv()
	if err != nil {
		return frameType, payload, err
	}
	plain, oerr := t.c.OpenBody(payload)
	if oerr != nil {
		// A stream cannot recover from an undecryptable frame: the peer either
		// has a different p2p-key or the stream was tampered with. Fail closed.
		return 0, nil, fmt.Errorf("frame: payload authentication failed (p2p-key mismatch or tampering): %w", oerr)
	}
	return frameType, plain, nil
}

func (t *cryptoTransport) Close() error { return t.base.Close() }

// IsStreamCarrier delegates the capability of the wrapped transport so the
// p2p layer still recognises stream carriers through the decorator.
func (t *cryptoTransport) IsStreamCarrier() bool {
	if sc, ok := t.base.(StreamCarrier); ok {
		return sc.IsStreamCarrier()
	}
	return false
}
