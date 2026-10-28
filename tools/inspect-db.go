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
		fmt.Fprintf(os.Stderr, "Failed to open db: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	fmt.Println("=== Database Buckets and Keys ===")
	db.View(func(tx *bolt.Tx) error {
		return tx.ForEach(func(name []byte, b *bolt.Bucket) error {
			fmt.Printf("\nBucket: %s\n", string(name))
			return b.ForEach(func(k, v []byte) error {
				valueStr := string(v)
				if len(valueStr) > 200 {
					valueStr = valueStr[:200] + "..."
				}
				fmt.Printf("  Key: %s = %s\n", string(k), valueStr)
				return nil
			})
		})
	})
}
