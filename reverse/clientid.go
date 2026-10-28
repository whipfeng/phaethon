package reverse

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"phaethon/db"
	"phaethon/mesh"
)

// GetReverseID returns the instance node-id for use as reverse proxy client identity.
// Priority: database node-id > legacy file reverse-id > generate new.
// If a legacy file is found, it is migrated to the database automatically.
func GetReverseID(dataDir string) (string, error) {
	// 1. Try database first
	nodeID, err := db.GetNodeID()
	if err != nil {
		return "", fmt.Errorf("load node-id from db fail: %w", err)
	}
	if nodeID != "" {
		return nodeID, nil
	}

	// 2. Try legacy file (for backwards compatibility before database migration)
	idFile := filepath.Join(dataDir, "reverse-id")
	if data, err := os.ReadFile(idFile); err == nil {
		id := strings.TrimSpace(string(data))
		if id != "" {
			// Migrate legacy file to database
			if err := db.PutNodeID(id); err != nil {
				return "", fmt.Errorf("migrate legacy reverse-id to db fail: %w", err)
			}
			// Remove legacy file after successful migration
			_ = os.Remove(idFile)
			return id, nil
		}
	}

	// 3. Generate a new node-id
	nodeID, err = mesh.GenerateNodeID()
	if err != nil {
		return "", fmt.Errorf("generate node-id fail: %w", err)
	}

	if err := db.PutNodeID(nodeID); err != nil {
		return "", fmt.Errorf("save node-id to db fail: %w", err)
	}
	return nodeID, nil
}
