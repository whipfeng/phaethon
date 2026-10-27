package main

import (
	"fmt"
	"os"

	bolt "go.etcd.io/bbolt"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: %s <db-path>\n", os.Args[0])
		os.Exit(1)
	}

	dbPath := os.Args[1]

	db, err := bolt.Open(dbPath, 0600, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	db.View(func(tx *bolt.Tx) error {
		fmt.Printf("=== Database: %s ===\n\n", dbPath)

		tx.ForEach(func(name []byte, b *bolt.Bucket) error {
			stats := b.Stats()
			fmt.Printf("Bucket: %s (keys: %d, depth: %d)\n", string(name), stats.KeyN, stats.Depth)

			// Show first 10 keys
			c := b.Cursor()
			count := 0
			for k, v := c.First(); k != nil && count < 10; k, v = c.Next() {
				valuePreview := ""
				if len(v) > 50 {
					valuePreview = string(v[:50]) + "..."
				} else {
					valuePreview = string(v)
				}
				fmt.Printf("  [%d] %s = %s\n", count, string(k), valuePreview)
				count++
			}
			if stats.KeyN > 10 {
				fmt.Printf("  ... and %d more keys\n", stats.KeyN-10)
			}
			fmt.Println()
			return nil
		})
		return nil
	})
}
