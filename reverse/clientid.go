package reverse

import (
	"fmt"

	"phaethon/db"
)

// GetReverseID returns the instance node-id used as the reverse proxy client
// identity. The node-id is owned by the mesh config; mesh init guarantees it
// is generated and persisted before any reverse client starts, so this
// function only reads and errors out if the invariant is violated.
func GetReverseID() (string, error) {
	meshCfg, err := db.GetMeshConfig()
	if err != nil {
		return "", fmt.Errorf("load mesh config for reverse-id fail: %w", err)
	}
	if meshCfg == nil || meshCfg.NodeID == "" {
		return "", fmt.Errorf("node-id not initialized: mesh init must run before reverse")
	}
	return meshCfg.NodeID, nil
}
