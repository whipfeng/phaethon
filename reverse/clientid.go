package reverse

import (
	"fmt"

	"phaethon/db"
	"phaethon/mesh"
)

// GetReverseID returns the instance node-id for use as reverse proxy client identity.
// If node-id does not exist, generates a new one and saves it.
func GetReverseID(dataDir string) (string, error) {
	nodeID, err := db.GetNodeID()
	if err != nil {
		return "", fmt.Errorf("load node-id from db fail: %w", err)
	}
	if nodeID != "" {
		return nodeID, nil
	}

	// Generate a new node-id
	nodeID, err = mesh.GenerateNodeID()
	if err != nil {
		return "", fmt.Errorf("generate node-id fail: %w", err)
	}

	if err := db.PutNodeID(nodeID); err != nil {
		return "", fmt.Errorf("save node-id to db fail: %w", err)
	}
	return nodeID, nil
}
