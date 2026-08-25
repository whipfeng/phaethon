package util

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Level 1: HTunnelCrypto (XChaCha20-Poly1305 AEAD for h_tunnel)
// ---------------------------------------------------------------------------

func TestHTunnelCrypto_New_EmptyPassword(t *testing.T) {
	c := NewHTunnelCrypto("")
	if c.IsEnabled() {
		t.Fatal("expected disabled for empty password")
	}
}

func TestHTunnelCrypto_New_WithPassword(t *testing.T) {
	c := NewHTunnelCrypto("test")
	if !c.IsEnabled() {
		t.Fatal("expected enabled for non-empty password")
	}
}

// -- Header round-trip -------------------------------------------------------

func TestHTunnelCrypto_SealHeader_OpenHeader_RoundTrip(t *testing.T) {
	tests := []string{
		"hello",
		"example.com",
		"example.com:443",
		strings.Repeat("a", 256),
		"",
	}
	for _, input := range tests {
		c := NewHTunnelCrypto("secret")
		enc := c.SealHeader(input)
		dec, err := c.OpenHeader(enc)
		if err != nil {
			t.Errorf("OpenHeader(%q) failed: %v", input, err)
			continue
		}
		if dec != input {
			t.Errorf("round-trip mismatch: got %q, want %q", dec, input)
		}
	}
}

func TestHTunnelCrypto_SealHeader_DifferentEachTime(t *testing.T) {
	c := NewHTunnelCrypto("secret")
	enc1 := c.SealHeader("hello")
	enc2 := c.SealHeader("hello")
	if enc1 == enc2 {
		t.Fatal("same input should produce different ciphertexts (random nonce)")
	}
}

func TestHTunnelCrypto_SealHeader_OutputIsBase64(t *testing.T) {
	c := NewHTunnelCrypto("secret")
	enc := c.SealHeader("hello")
	// Must be valid base64
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("SealHeader output is not valid base64: %v", err)
	}
	// Binary payload = nonce(24) + plaintext + tag(16) = 24 + 5 + 16 = 45
	if len(raw) < htNonceSize+htOverhead {
		t.Fatalf("SealHeader output too short: %d bytes", len(raw))
	}
}

func TestHTunnelCrypto_OpenHeader_RejectsTampered(t *testing.T) {
	c := NewHTunnelCrypto("secret")
	enc := c.SealHeader("hello")

	// Flip a byte in the base64-encoded string
	b := []byte(enc)
	b[len(b)/2] ^= 0x01
	_, err := c.OpenHeader(string(b))
	if err == nil {
		t.Fatal("OpenHeader should reject tampered base64")
	}
}

func TestHTunnelCrypto_OpenHeader_RejectsInvalidBase64(t *testing.T) {
	c := NewHTunnelCrypto("secret")
	_, err := c.OpenHeader("!!!not-base64!!!")
	if err == nil {
		t.Fatal("OpenHeader should reject invalid base64")
	}
}

func TestHTunnelCrypto_OpenHeader_WrongPassword(t *testing.T) {
	c1 := NewHTunnelCrypto("alice")
	c2 := NewHTunnelCrypto("bob")
	enc := c1.SealHeader("hello")
	_, err := c2.OpenHeader(enc)
	if err == nil {
		t.Fatal("OpenHeader should fail with wrong password")
	}
}

// -- Body round-trip ---------------------------------------------------------

func TestHTunnelCrypto_SealBody_OpenBody_RoundTrip(t *testing.T) {
	tests := []int{0, 1, 64, 512, 1400, 65535}
	for _, size := range tests {
		input := make([]byte, size)
		for i := range input {
			input[i] = byte(i & 0xff)
		}
		c := NewHTunnelCrypto("secret")
		enc := c.SealBody(input)
		dec, err := c.OpenBody(enc)
		if err != nil {
			t.Errorf("OpenBody(size=%d) failed: %v", size, err)
			continue
		}
		if !bytes.Equal(dec, input) {
			t.Errorf("round-trip mismatch for size=%d", size)
		}
	}
}

func TestHTunnelCrypto_SealBody_OutputSize(t *testing.T) {
	c := NewHTunnelCrypto("secret")
	input := []byte("hello world") // 11 bytes
	enc := c.SealBody(input)
	// Expected: nonce(24) + input(11) + tag(16) = 51
	expectedLen := htNonceSize + len(input) + htOverhead
	if len(enc) != expectedLen {
		t.Errorf("SealBody output size: got %d, want %d", len(enc), expectedLen)
	}
}

func TestHTunnelCrypto_SealBody_ProducesDifferentOutput(t *testing.T) {
	c := NewHTunnelCrypto("secret")
	input := []byte("hello")
	enc1 := c.SealBody(input)
	enc2 := c.SealBody(input)
	if bytes.Equal(enc1, enc2) {
		t.Fatal("same input should produce different ciphertexts (random nonce)")
	}
}

func TestHTunnelCrypto_OpenBody_RejectsTampered(t *testing.T) {
	c := NewHTunnelCrypto("secret")
	enc := c.SealBody([]byte("hello world"))

	// Flip a byte AFTER the nonce (modify ciphertext, not nonce)
	tampered := make([]byte, len(enc))
	copy(tampered, enc)
	tampered[htNonceSize+2] ^= 0x01

	_, err := c.OpenBody(tampered)
	if err == nil {
		t.Fatal("OpenBody should reject tampered ciphertext")
	}
}

func TestHTunnelCrypto_OpenBody_RejectsTruncated(t *testing.T) {
	c := NewHTunnelCrypto("secret")

	// Too short for nonce+tag
	_, err := c.OpenBody([]byte{0x00})
	if err == nil {
		t.Fatal("OpenBody should reject 1-byte ciphertext")
	}
	_, err = c.OpenBody(make([]byte, htNonceSize-1))
	if err == nil {
		t.Fatalf("OpenBody should reject %d-byte ciphertext", htNonceSize-1)
	}
	// Exactly nonce size but no tag
	_, err = c.OpenBody(make([]byte, htNonceSize+htOverhead-1))
	if err == nil {
		t.Fatalf("OpenBody should reject %d-byte ciphertext (no tag)", htNonceSize+htOverhead-1)
	}
}

func TestHTunnelCrypto_OpenBody_WrongPassword(t *testing.T) {
	c1 := NewHTunnelCrypto("alice")
	c2 := NewHTunnelCrypto("bob")
	enc := c1.SealBody([]byte("hello"))
	_, err := c2.OpenBody(enc)
	if err == nil {
		t.Fatal("OpenBody should fail with wrong password")
	}
}

func TestHTunnelCrypto_SealBody_NilAndEmpty(t *testing.T) {
	c := NewHTunnelCrypto("secret")

	// nil → nil (no overhead)
	enc := c.SealBody(nil)
	if enc != nil {
		t.Errorf("SealBody(nil) should return nil, got %d bytes", len(enc))
	}

	// empty slice → empty slice (no overhead)
	enc = c.SealBody([]byte{})
	if len(enc) != 0 {
		t.Errorf("SealBody([]byte{}) should return empty, got %d bytes", len(enc))
	}
}

// -- Disabled (no password) passthrough --------------------------------------

func TestHTunnelCrypto_Disabled_Passthrough(t *testing.T) {
	c := NewHTunnelCrypto("")

	// Header passthrough
	in := "example.com"
	enc := c.SealHeader(in)
	if enc != in {
		t.Errorf("disabled SealHeader should return input unchanged")
	}
	dec, err := c.OpenHeader(in)
	if err != nil || dec != in {
		t.Errorf("disabled OpenHeader should return input unchanged")
	}

	// Body passthrough
	body := []byte("hello")
	encb := c.SealBody(body)
	if !bytes.Equal(encb, body) {
		t.Errorf("disabled SealBody should return input unchanged")
	}
	decb, err := c.OpenBody(body)
	if err != nil || !bytes.Equal(decb, body) {
		t.Errorf("disabled OpenBody should return input unchanged")
	}

	// Empty body passthrough
	encb = c.SealBody(nil)
	if encb != nil {
		t.Errorf("disabled SealBody(nil) should return nil")
	}
}
