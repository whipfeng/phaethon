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

	fmt.Println("Migrating BucketConfig to separate buckets...")

	err := db.Update(func(tx *bolt.Tx) error {
		oldBucket := tx.Bucket([]byte("config"))
		if oldBucket == nil {
			fmt.Println("✓ BucketConfig not found, skipping migration")
			return nil
		}

		// Read all config data
		configs := make(map[string][]byte)
		err := oldBucket.ForEach(func(k, v []byte) error {
			configs[string(k)] = v
			return nil
		})
		if err != nil {
			return fmt.Errorf("failed to read config: %w", err)
		}

		fmt.Printf("Found %d config entries\n", len(configs))

		// Mapping from old keys to new buckets
		bucketMapping := map[string][]byte{
			"admin":            db.BucketAdminConfig,
			"tun":              db.BucketTUNConfig,
			"mesh":             db.BucketMeshConfig,
			"interactive":      db.BucketInteractiveConfig,
			"udp-port-range":   db.BucketPortRangeConfig,
			"tcp-port-range":   db.BucketPortRangeConfig,
		}

		// Create new buckets and migrate data
		migratedCount := 0
		for oldKey, data := range configs {
			newBucketName, ok := bucketMapping[oldKey]
			if !ok {
				fmt.Printf("⚠ Unknown config key: %s, skipping\n", oldKey)
				continue
			}

			newBucket, err := tx.CreateBucketIfNotExists(newBucketName)
			if err != nil {
				return fmt.Errorf("failed to create bucket %s: %w", string(newBucketName), err)
			}

			// Store data with the original key name (not "data")
			if err := newBucket.Put([]byte(oldKey), data); err != nil {
				return fmt.Errorf("failed to write to bucket %s: %w", string(newBucketName), err)
			}

			fmt.Printf("✓ Migrated %s → %s (key: %s)\n", oldKey, string(newBucketName), oldKey)
			migratedCount++
		}

		// Delete old bucket
		if err := tx.DeleteBucket([]byte("config")); err != nil {
			return fmt.Errorf("failed to delete old bucket: %w", err)
		}

		fmt.Printf("\n✓ Successfully migrated %d config entries\n", migratedCount)
		fmt.Println("✓ Old BucketConfig deleted")

		return nil
	})

	if err != nil {
		fmt.Fprintf(os.Stderr, "Migration failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("\n✓ Config bucket migration completed successfully!")
}
