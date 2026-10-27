package main

import (
	"encoding/json"
	"fmt"
	"os"

	bolt "go.etcd.io/bbolt"
	"phaethon/db"
)

type MeshConfig struct {
	NodeID string `json:"node-id"`
	Subnet string `json:"subnet"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: %s <db-path>\n", os.Args[0])
		os.Exit(1)
	}

	dbPath := os.Args[1]

	if err := db.Init(dbPath); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to init database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	fmt.Println("Migrating node-id to instance-identity bucket...")

	err := db.Update(func(tx *bolt.Tx) error {
		// Check if node-id already exists
		identityBucket := tx.Bucket(db.BucketInstanceIdentity)
		if identityBucket == nil {
			return fmt.Errorf("instance-identity bucket not found")
		}

		existingNodeID := identityBucket.Get([]byte("node-id"))
		if existingNodeID != nil {
			fmt.Printf("✓ node-id already exists: %s\n", string(existingNodeID))
			return nil
		}

		// Read mesh-config to extract node-id
		meshConfigBucket := tx.Bucket(db.BucketMeshConfig)
		if meshConfigBucket == nil {
			return fmt.Errorf("mesh-config bucket not found")
		}

		meshData := meshConfigBucket.Get([]byte("mesh"))
		if meshData == nil {
			return fmt.Errorf("mesh config not found")
		}

		var meshConfig MeshConfig
		if err := json.Unmarshal(meshData, &meshConfig); err != nil {
			return fmt.Errorf("failed to parse mesh config: %w", err)
		}

		if meshConfig.NodeID == "" {
			return fmt.Errorf("node-id is empty in mesh config")
		}

		// Store node-id in instance-identity bucket
		if err := identityBucket.Put([]byte("node-id"), []byte(meshConfig.NodeID)); err != nil {
			return fmt.Errorf("failed to store node-id: %w", err)
		}

		fmt.Printf("✓ Migrated node-id '%s' to instance-identity bucket\n", meshConfig.NodeID)
		return nil
	})

	if err != nil {
		fmt.Fprintf(os.Stderr, "Migration failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("✓ Node-id migration completed successfully!")
}
