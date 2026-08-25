package reverse

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const reverseIDFile = "reverse-id"

// GetReverseID loads the reverse identity from <dataDir>/reverse-id.
// If the file does not exist, a new UUID-like identifier is generated and saved.
func GetReverseID(dataDir string) (string, error) {
	idFile := filepath.Join(dataDir, reverseIDFile)

	if data, err := os.ReadFile(idFile); err == nil {
		id := strings.TrimSpace(string(data))
		if id != "" {
			return id, nil
		}
	}

	// Generate a new 128-bit random identifier (UUID v4 style, no dashes for compactness)
	id, err := generateID()
	if err != nil {
		return "", fmt.Errorf("generate reverse id fail: %w", err)
	}

	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return "", fmt.Errorf("create data dir fail: %w", err)
	}
	if err := os.WriteFile(idFile, []byte(id), 0644); err != nil {
		return "", fmt.Errorf("save reverse id fail: %w", err)
	}
	return id, nil
}

// GenerateReverseID creates a new 128-bit random identifier (UUID v4 style,
// no dashes) suitable for use as a reverse client identity.
func GenerateReverseID() (string, error) {
	return generateID()
}

func generateID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	// Set version (4) and variant bits for valid UUID v4
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b), nil
}
