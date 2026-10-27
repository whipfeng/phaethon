package main

import (
	"encoding/binary"
	"fmt"
	"os"

	bolt "go.etcd.io/bbolt"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: dump-versions <db-path>")
		os.Exit(1)
	}

	dbPath := os.Args[1]
	db, err := bolt.Open(dbPath, 0600, nil)
	if err != nil {
		fmt.Printf("Failed to open database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	fmt.Println("=== Versions Bucket ===")
	db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("versions"))
		if bucket == nil {
			fmt.Println("Bucket 'versions' not found")
			return nil
		}

		count := 0
		bucket.ForEach(func(k, v []byte) error {
			if len(v) == 8 {
				version := binary.LittleEndian.Uint64(v)
				fmt.Printf("  %s: %d\n", string(k), version)
			} else {
				fmt.Printf("  %s: <invalid length %d>\n", string(k), len(v))
			}
			count++
			return nil
		})
		fmt.Printf("Total: %d keys\n", count)
		return nil
	})
}
