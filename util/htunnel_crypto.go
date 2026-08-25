package util

import (
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"

	"golang.org/x/crypto/chacha20poly1305"
)

// HTunnelCrypto wraps XChaCha20-Poly1305 AEAD for h_tunnel HTTP encryption.
// Replaces the legacy RC4 cipher with authenticated encryption.
//
// When enabled:
//   - Headers are encrypted then base64-encoded.
//   - Bodies are encrypted as raw binary with nonce prefix.
//
// When disabled (empty password):
//   - All methods are identity passthroughs with zero overhead.
type HTunnelCrypto struct {
	aead    cipher.AEAD
	enabled bool
}

const htNonceSize = chacha20poly1305.NonceSizeX // 24
const htOverhead = 16                           // Poly1305 tag

// ErrHTAuthFailed indicates AEAD authentication failed.
var ErrHTAuthFailed = errors.New("htunnel_crypto: authentication failed")

// NewHTunnelCrypto creates an HTunnelCrypto from a password string.
// The password is hashed with SHA-256 to produce a 32-byte key for XChaCha20-Poly1305.
// An empty password disables encryption entirely (all methods become passthrough).
func NewHTunnelCrypto(password string) *HTunnelCrypto {
	if password == "" {
		return &HTunnelCrypto{enabled: false}
	}
	key := sha256.Sum256([]byte(password))
	aead, err := chacha20poly1305.NewX(key[:])
	if err != nil {
		// NewX only fails on wrong key size (must be 32).
		// We always pass [32]byte, so this is unreachable.
		panic("htunnel_crypto: chacha20poly1305.NewX: " + err.Error())
	}
	return &HTunnelCrypto{aead: aead, enabled: true}
}

// IsEnabled reports whether encryption is active.
func (c *HTunnelCrypto) IsEnabled() bool {
	return c.enabled
}

// SealHeader encrypts plaintext and returns a base64-encoded string.
// Format: base64([nonce:24][ciphertext+tag])
//
// When disabled, returns plaintext unchanged.
func (c *HTunnelCrypto) SealHeader(plaintext string) string {
	if !c.enabled {
		return plaintext
	}
	nonce := make([]byte, htNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		panic("htunnel_crypto: rand.Read failed: " + err.Error())
	}
	out := c.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(out)
}

// OpenHeader decodes a base64-encoded ciphertext and returns the plaintext.
// Returns ErrHTAuthFailed on authentication failure or malformed input.
//
// When disabled, returns encoded unchanged.
func (c *HTunnelCrypto) OpenHeader(encoded string) (string, error) {
	if !c.enabled {
		return encoded, nil
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", ErrHTAuthFailed
	}
	if len(raw) < htNonceSize+htOverhead {
		return "", ErrHTAuthFailed
	}
	nonce := raw[:htNonceSize]
	ciphertext := raw[htNonceSize:]
	plain, err := c.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", ErrHTAuthFailed
	}
	return string(plain), nil
}

// SealBody encrypts plaintext and returns raw binary ciphertext.
// Format: [nonce:24][ciphertext+tag]
// Empty input produces empty output (no overhead).
//
// When disabled, returns plaintext unchanged.
func (c *HTunnelCrypto) SealBody(plaintext []byte) []byte {
	if !c.enabled || len(plaintext) == 0 {
		return plaintext
	}
	nonce := make([]byte, htNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		panic("htunnel_crypto: rand.Read failed: " + err.Error())
	}
	return c.aead.Seal(nonce, nonce, plaintext, nil)
}

// OpenBody decrypts ciphertext and verifies authenticity.
// Input format: [nonce:24][ciphertext+tag]
// Empty input produces empty output (no overhead).
//
// When disabled, returns ciphertext unchanged.
func (c *HTunnelCrypto) OpenBody(ciphertext []byte) ([]byte, error) {
	if !c.enabled || len(ciphertext) == 0 {
		return ciphertext, nil
	}
	if len(ciphertext) < htNonceSize+htOverhead {
		return nil, ErrHTAuthFailed
	}
	nonce := ciphertext[:htNonceSize]
	encrypted := ciphertext[htNonceSize:]
	return c.aead.Open(nil, nonce, encrypted, nil)
}
