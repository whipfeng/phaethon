package mesh

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
)

// MeshState holds the persistent mesh node state.
type MeshState struct {
	NodeID string `json:"nodeId"`
	Subnet string `json:"subnet,omitempty"`
}

// LoadState loads the mesh state from <dataDir>/mesh-state.json.
// Returns nil if the file does not exist.
func LoadState(dataDir string) (*MeshState, error) {
	stateFile := filepath.Join(dataDir, "mesh-state.json")
	data, err := os.ReadFile(stateFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read mesh state fail: %w", err)
	}
	var state MeshState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse mesh state fail: %w", err)
	}
	return &state, nil
}

// SaveState persists the mesh state to <dataDir>/mesh-state.json.
func SaveState(dataDir string, state *MeshState) error {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return fmt.Errorf("create data dir fail: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal mesh state fail: %w", err)
	}
	stateFile := filepath.Join(dataDir, "mesh-state.json")
	if err := os.WriteFile(stateFile, data, 0644); err != nil {
		return fmt.Errorf("save mesh state fail: %w", err)
	}
	return nil
}

// GenerateNodeID generates a random 64-bit integer as a string.
func GenerateNodeID() (string, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).SetUint64(^uint64(0)))
	if err != nil {
		return "", fmt.Errorf("generate node id fail: %w", err)
	}
	return strconv.FormatUint(n.Uint64(), 10), nil
}

// AllocateSubnet randomly selects a /24 from 100.64.0.0/16 that is not in usedSubnets.
func AllocateSubnet(usedSubnets map[string]bool) (string, error) {
	const poolBase = 0x6440 // 100.64 in hex
	for attempts := 0; attempts < 1000; attempts++ {
		n, err := rand.Int(rand.Reader, big.NewInt(256))
		if err != nil {
			return "", fmt.Errorf("allocate subnet fail: %w", err)
		}
		candidate := fmt.Sprintf("100.64.%d.0/24", n.Int64())
		if !usedSubnets[candidate] {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("allocate subnet fail: no available /24 in 100.64.0.0/16 after 1000 attempts")
}

