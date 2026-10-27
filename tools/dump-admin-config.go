package main

import (
	"encoding/json"
	"fmt"
	"os"

	bolt "go.etcd.io/bbolt"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: dump-admin-config <db-path>")
		os.Exit(1)
	}

	dbPath := os.Args[1]
	db, err := bolt.Open(dbPath, 0600, nil)
	if err != nil {
		fmt.Printf("Failed to open database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	fmt.Println("=== Admin Config Bucket ===")
	db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("admin-config"))
		if bucket == nil {
			fmt.Println("Bucket 'admin-config' not found")
			return nil
		}

		bucket.ForEach(func(k, v []byte) error {
			fmt.Printf("Key: %s\n", string(k))
			var prettyJSON map[string]interface{}
			if err := json.Unmarshal(v, &prettyJSON); err == nil {
				formatted, _ := json.MarshalIndent(prettyJSON, "  ", "  ")
				fmt.Printf("Value: %s\n", string(formatted))
			} else {
				fmt.Printf("Value: %s\n", string(v))
			}
			fmt.Println("---")
			return nil
		})
		return nil
	})
}
