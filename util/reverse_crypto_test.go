package util

import (
	"bytes"
	"crypto/rand"
	"testing"
)

// ============================================================
//  Level 1 Unit Tests: ReverseCrypto (XChaCha20-Poly1305 AEAD)
// ============================================================

func TestReverseCrypto_SealOpenRoundTrip(t *testing.T) {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		t.Fatal(err)
	}
	crypto := NewReverseCrypto(key)

	// Test various payload sizes including edge cases
	sizes := []int{0, 1, 16, 64, 256, 1500, 8192}
	for _, size := range sizes {
		plaintext := make([]byte, size)
		if size > 0 {
			if _, err := rand.Read(plaintext); err != nil {
				t.Fatal(err)
			}
		}

		ciphertext := crypto.Seal(plaintext)
		decrypted, err := crypto.Open(ciphertext)
		if err != nil {
			t.Fatalf("size=%d: Open failed: %v", size, err)
		}
		if !bytes.Equal(decrypted, plaintext) {
			t.Fatalf("size=%d: round-trip mismatch\n  sent: %x\n  got:  %x", size, plaintext[:min(size, 32)], decrypted[:min(size, 32)])
		}

		// Verify ciphertext format: NonceSize bytes of nonce + plaintext + OverheadSize
		expectedLen := NonceSize + size + OverheadSize
		if len(ciphertext) != expectedLen {
			t.Errorf("size=%d: ciphertext length = %d, expected %d", size, len(ciphertext), expectedLen)
		}
	}
}

func TestReverseCrypto_SealProducesDifferentCiphertexts(t *testing.T) {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		t.Fatal(err)
	}
	crypto := NewReverseCrypto(key)

	payload := []byte("hello reverse crypto")
	ct1 := crypto.Seal(payload)
	ct2 := crypto.Seal(payload)

	// Same plaintext, different ciphertext due to random nonce
	if bytes.Equal(ct1, ct2) {
		t.Error("two Seal calls produced identical ciphertext — nonce randomness not working")
	}

	// Both should decrypt correctly
	for i, ct := range [][]byte{ct1, ct2} {
		pt, err := crypto.Open(ct)
		if err != nil {
			t.Fatalf("seal #%d: Open failed: %v", i+1, err)
		}
		if !bytes.Equal(pt, payload) {
			t.Fatalf("seal #%d: decrypted payload mismatch", i+1)
		}
	}
}

func TestReverseCrypto_OpenRejectsTampered(t *testing.T) {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		t.Fatal(err)
	}
	crypto := NewReverseCrypto(key)

	payload := []byte("tamper-me")
	ciphertext := crypto.Seal(payload)

	// Flip a byte in the encrypted portion (past the nonce)
	idx := NonceSize + 1
	if idx < len(ciphertext) {
		ciphertext[idx] ^= 0x01
	}

	_, err := crypto.Open(ciphertext)
	if err == nil {
		t.Fatal("Open accepted tampered ciphertext — should have rejected")
	}
	if err != ErrAuthFailed {
		t.Logf("Open returned error: %v (expected ErrAuthFailed)", err)
	}
}

func TestReverseCrypto_OpenRejectsTruncated(t *testing.T) {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		t.Fatal(err)
	}
	crypto := NewReverseCrypto(key)

	payload := []byte("data")
	ciphertext := crypto.Seal(payload)
	minLen := NonceSize + OverheadSize

	for _, badLen := range []int{0, 1, NonceSize - 1, NonceSize, NonceSize + OverheadSize - 1} {
		if badLen >= len(ciphertext) {
			continue
		}
		truncated := ciphertext[:badLen]
		_, err := crypto.Open(truncated)
		if err == nil {
			t.Fatalf("Open accepted truncated ciphertext (len=%d) — should have rejected", badLen)
		}
	}
	_ = minLen
}

func TestReverseCrypto_OpenRejectsWrongKey(t *testing.T) {
	var key1, key2 [32]byte
	if _, err := rand.Read(key1[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(key2[:]); err != nil {
		t.Fatal(err)
	}
	// Ensure keys differ
	key1[0] ^= 0xFF

	crypto1 := NewReverseCrypto(key1)
	crypto2 := NewReverseCrypto(key2)

	payload := []byte("wrong-key-test")
	ciphertext := crypto1.Seal(payload)

	_, err := crypto2.Open(ciphertext)
	if err == nil {
		t.Fatal("crypto2 accepted crypto1's ciphertext — key isolation broken")
	}
	if err != ErrAuthFailed {
		t.Logf("Open with wrong key returned: %v (expected ErrAuthFailed)", err)
	}
}

func TestReverseCrypto_EmptyPayload(t *testing.T) {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		t.Fatal(err)
	}
	crypto := NewReverseCrypto(key)

	// Empty payload: seal + open round-trip
	ciphertext := crypto.Seal(nil)
	plaintext, err := crypto.Open(ciphertext)
	if err != nil {
		t.Fatalf("Open empty payload failed: %v", err)
	}
	if len(plaintext) != 0 {
		t.Fatalf("expected empty payload, got %d bytes", len(plaintext))
	}

	// Empty ciphertext edge case
	_, err = crypto.Open(nil)
	if err == nil {
		t.Fatal("Open nil ciphertext should fail")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
