package mesh

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"net"
	"strconv"
)

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

