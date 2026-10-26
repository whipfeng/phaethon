package mesh

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"net"
	"strconv"

	"phaethon/config"
	"phaethon/db"
)

// MeshState holds the persistent mesh node state.
type MeshState struct {
	NodeID string `json:"nodeId"`
	Subnet string `json:"subnet,omitempty"`
}

// LoadState loads the mesh state from database.
// Returns nil if no state exists.
func LoadState(dataDir string) (*MeshState, error) {
	nodeID, err := db.GetNodeID()
	if err != nil {
		return nil, fmt.Errorf("load node-id from db fail: %w", err)
	}
	if nodeID == "" {
		return nil, nil
	}
	
	// Load subnet from MeshConfig
	cfg, err := db.GetMeshConfig()
	if err != nil {
		return nil, fmt.Errorf("load mesh config from db fail: %w", err)
	}
	
	subnet := ""
	if cfg != nil {
		subnet = cfg.Subnet
	}
	
	return &MeshState{
		NodeID: nodeID,
		Subnet: subnet,
	}, nil
}

// SaveState persists the mesh state to database.
func SaveState(dataDir string, state *MeshState) error {
	// Save node-id to state bucket
	if err := db.PutNodeID(state.NodeID); err != nil {
		return fmt.Errorf("save node-id to db fail: %w", err)
	}
	
	// Save subnet to MeshConfig
	cfg, err := db.GetMeshConfig()
	if err != nil {
		return fmt.Errorf("load mesh config fail: %w", err)
	}
	if cfg == nil {
		cfg = &config.MeshConfig{}
	}
	cfg.Subnet = state.Subnet
	if err := db.PutMeshConfig(cfg); err != nil {
		return fmt.Errorf("save mesh config to db fail: %w", err)
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

