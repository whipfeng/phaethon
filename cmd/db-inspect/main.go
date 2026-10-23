package main

import (
	"encoding/json"
	"fmt"
	"os"

	bolt "go.etcd.io/bbolt"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: db-inspect <db-path>")
		os.Exit(1)
	}

	dbPath := os.Args[1]
	db, err := bolt.Open(dbPath, 0600, nil)
	if err != nil {
		fmt.Printf("Failed to open database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	fmt.Printf("Database: %s\n\n", dbPath)

	err = db.View(func(tx *bolt.Tx) error {
		// List all buckets
		fmt.Println("=== Buckets ===")
		tx.ForEach(func(name []byte, b *bolt.Bucket) error {
			stats := b.Stats()
			fmt.Printf("- %s (keys: %d)\n", string(name), stats.KeyN)
			return nil
		})
		fmt.Println()

		// Check config buckets
		buckets := []string{
			"config",
			"proxies",
			"proxy-groups",
			"subscriptions",
			"rules",
			"mappings",
			"resolvers",
			"reverse-configs",
		}

		for _, bucketName := range buckets {
			b := tx.Bucket([]byte(bucketName))
			if b == nil {
				fmt.Printf("=== %s: NOT FOUND ===\n\n", bucketName)
				continue
			}

			stats := b.Stats()
			fmt.Printf("=== %s: %d keys ===\n", bucketName, stats.KeyN)

			if stats.KeyN == 0 {
				fmt.Println("(empty)")
			} else if stats.KeyN <= 10 {
				// Show all keys if <= 10
				b.ForEach(func(k, v []byte) error {
					fmt.Printf("  Key: %s\n", string(k))
					// Try to pretty print JSON
					var data interface{}
					if err := json.Unmarshal(v, &data); err == nil {
						pretty, _ := json.MarshalIndent(data, "    ", "  ")
						fmt.Printf("  Value: %s\n", string(pretty))
					} else {
						fmt.Printf("  Value: %s\n", string(v))
					}
					return nil
				})
			} else {
				// Show first 5 keys if > 10
				count := 0
				b.ForEach(func(k, v []byte) error {
					if count >= 5 {
						return nil
					}
					fmt.Printf("  Key: %s\n", string(k))
					count++
					return nil
				})
				fmt.Printf("  ... and %d more keys\n", stats.KeyN-5)
			}
			fmt.Println()
		}

		return nil
	})

	if err != nil {
		fmt.Printf("Error reading database: %v\n", err)
		os.Exit(1)
	}
}
