package mesh

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
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

// AllocateSubnet randomly selects a subnet of subnetPrefixLen from network that is not in usedSubnets.
// For example, network=100.0.0.0/8 with subnetPrefixLen=16 allocates a random /16 from the /8 range.
func AllocateSubnet(network *net.IPNet, subnetPrefixLen int, usedSubnets map[string]bool) (string, error) {
	netBase := network.IP.To4()
	if netBase == nil {
		return "", fmt.Errorf("allocate subnet fail: network is not IPv4")
	}
	networkPrefixLen, totalBits := network.Mask.Size()
	if subnetPrefixLen <= networkPrefixLen || subnetPrefixLen >= totalBits {
		return "", fmt.Errorf("allocate subnet fail: invalid subnet prefix /%d for network /%d", subnetPrefixLen, networkPrefixLen)
	}

	subnetBits := subnetPrefixLen - networkPrefixLen
	numSubnets := uint32(1) << uint(subnetBits)
	baseUint32 := uint32(netBase[0])<<24 | uint32(netBase[1])<<16 | uint32(netBase[2])<<8 | uint32(netBase[3])
	hostBits := uint(totalBits - subnetPrefixLen)
	subnetSize := uint32(1) << hostBits

	maxAttempts := int(numSubnets)
	if maxAttempts > 1000 {
		maxAttempts = 1000
	}
	for attempts := 0; attempts < maxAttempts; attempts++ {
		idx, err := rand.Int(rand.Reader, big.NewInt(int64(numSubnets)))
		if err != nil {
			return "", fmt.Errorf("allocate subnet fail: %w", err)
		}
		subnetBase := baseUint32 + uint32(idx.Int64())*subnetSize
		ip := net.IPv4(byte(subnetBase>>24), byte(subnetBase>>16), byte(subnetBase>>8), byte(subnetBase))
		candidate := fmt.Sprintf("%s/%d", ip.String(), subnetPrefixLen)
		if !usedSubnets[candidate] {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("allocate subnet fail: no available /%d in %s after %d attempts", subnetPrefixLen, network.String(), maxAttempts)
}

