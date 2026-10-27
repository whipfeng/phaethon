package main

import (
	"fmt"
	"os"

	bolt "go.etcd.io/bbolt"
	"phaethon/db"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: %s <db-path>\n", os.Args[0])
		os.Exit(1)
	}

	dbPath := os.Args[1]

	// Initialize database
	if err := db.Init(dbPath); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to init database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	fmt.Println("Migrating BucketState to separate buckets...")

	err := db.Update(func(tx *bolt.Tx) error {
		oldBucket := tx.Bucket([]byte("state"))
		if oldBucket == nil {
			fmt.Println("✓ BucketState not found, skipping migration")
			return nil
		}

		// Read all state data
		states := make(map[string][]byte)
		err := oldBucket.ForEach(func(k, v []byte) error {
			states[string(k)] = v
			return nil
		})
		if err != nil {
			return fmt.Errorf("failed to read state: %w", err)
		}

		fmt.Printf("Found %d state entries\n", len(states))

		// Identity keys (go to BucketInstanceIdentity)
		identityKeys := map[string]bool{
			"node-id":    true,
			"reverse-id": true,
		}

		// Create new buckets
		identityBucket, err := tx.CreateBucketIfNotExists(db.BucketInstanceIdentity)
		if err != nil {
			return fmt.Errorf("failed to create identity bucket: %w", err)
		}

		runtimeBucket, err := tx.CreateBucketIfNotExists(db.BucketRuntimeState)
		if err != nil {
			return fmt.Errorf("failed to create runtime bucket: %w", err)
		}

		// Migrate data
		identityCount := 0
		runtimeCount := 0

		for key, data := range states {
			if identityKeys[key] {
				// Identity data
				if err := identityBucket.Put([]byte(key), data); err != nil {
					return fmt.Errorf("failed to write identity data: %w", err)
				}
				fmt.Printf("✓ Migrated %s → instance-identity\n", key)
				identityCount++
			} else {
				// Runtime data
				if err := runtimeBucket.Put([]byte(key), data); err != nil {
					return fmt.Errorf("failed to write runtime data: %w", err)
				}
				fmt.Printf("✓ Migrated %s → runtime-state\n", key)
				runtimeCount++
			}
		}

		// Delete old bucket
		if err := tx.DeleteBucket([]byte("state")); err != nil {
			return fmt.Errorf("failed to delete old bucket: %w", err)
		}

		fmt.Printf("\n✓ Successfully migrated %d identity entries, %d runtime entries\n", identityCount, runtimeCount)
		fmt.Println("✓ Old BucketState deleted")

		return nil
	})

	if err != nil {
		fmt.Fprintf(os.Stderr, "Migration failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("\n✓ State bucket migration completed successfully!")
}
