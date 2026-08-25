package util

import (
	"crypto/cipher"
	"crypto/rand"
	"errors"

	"golang.org/x/crypto/chacha20poly1305"
)

// ReverseCrypto wraps ChaCha20-Poly1305-AEAD for reverse UDP frame encryption.
// Uses extended-nonce variant (X) to support random 12-byte nonces per packet
// without requiring counter synchronization between peers.
type ReverseCrypto struct {
	aead cipher.AEAD
}

// NewReverseCrypto creates a ReverseCrypto instance from a 32-byte key.
func NewReverseCrypto(key [32]byte) *ReverseCrypto {
	aead, err := chacha20poly1305.NewX(key[:])
	if err != nil {
		// chacha20poly1305.NewX only fails on wrong key size (must be 32).
		// Since we enforce [32]byte at the type level, this should never happen.
		panic("reverse_crypto: invalid key size: " + err.Error())
	}
	return &ReverseCrypto{aead: aead}
}

// NonceSize is the length of the random nonce prepended to each ciphertext.
// XChaCha20-Poly1305 uses a 24-byte nonce (192-bit), which is safe for random generation
// without risk of collisions even at extremely high packet rates.
const NonceSize = 24

// OverheadSize is the Poly1305 authentication tag size appended by AEAD.
const OverheadSize = 16

// Seal encrypts plaintext using ChaCha20-Poly1305-AEAD with a random nonce.
// Returns: [Nonce(24B)] + [Ciphertext + Tag(16B)]
//
// The nonce is generated randomly per packet (prefix mode). Collision probability
// is negligible: at 100K pps, birthday bound ~2^48 gives ~8900 years before p=10^-6.
func (c *ReverseCrypto) Seal(plaintext []byte) []byte {
	nonce := make([]byte, NonceSize)
	if _, err := rand.Read(nonce); err != nil {
		// If CSPRNG fails, we cannot safely continue — abort rather than
		// producing a predictable nonce that would compromise confidentiality.
		panic("reverse_crypto: rand.Read failed: " + err.Error())
	}
	// Seal appends the 16-byte tag to ciphertext internally.
	return c.aead.Seal(nonce, nonce, plaintext, nil)
}

// Open decrypts ciphertext and verifies authenticity.
// Input must be: [Nonce(24B)] + [Ciphertext + Tag(16B)]
// Returns decrypted plaintext on success, or error on auth failure / malformed input.
//
// Callers should silently drop packets on auth failure — see design docs for rationale.
func (c *ReverseCrypto) Open(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < NonceSize+OverheadSize {
		return nil, ErrAuthFailed
	}
	nonce := ciphertext[:NonceSize]
	encrypted := ciphertext[NonceSize:]
	return c.aead.Open(nil, nonce, encrypted, nil)
}

// ErrAuthFailed indicates AEAD authentication tag verification failed.
// The packet should be silently dropped (no response to potential attacker).
var ErrAuthFailed = errors.New("reverse_crypto: authentication failed")
