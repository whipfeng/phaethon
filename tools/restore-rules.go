package main

import (
	"encoding/json"
	"fmt"
	"os"

	"phaethon/db"

	"go.etcd.io/bbolt"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: restore-rules <backup-db-path>")
		os.Exit(1)
	}

	backupPath := os.Args[1]

	// Open backup database
	backupDB, err := bbolt.Open(backupPath, 0600, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open backup: %v\n", err)
		os.Exit(1)
	}
	defer backupDB.Close()

	// Read rules from backup
	var rules []string
	err = backupDB.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("rules"))
		if b == nil {
			return fmt.Errorf("rules bucket not found")
		}
		data := b.Get([]byte("__all_rules__"))
		if data == nil {
			return fmt.Errorf("rules list not found")
		}
		return json.Unmarshal(data, &rules)
	})

	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read rules from backup: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Found %d rules in backup\n", len(rules))

	// Clear current rules
	err = db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("rules"))
		if b == nil {
			return fmt.Errorf("rules bucket not found in current DB")
		}
		return b.Put([]byte("list"), []byte("[]"))
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to clear current rules: %v\n", err)
		os.Exit(1)
	}

	// Insert rules from backup
	for i, rule := range rules {
		err = db.InsertRule(i, rule)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to insert rule %d: %v\n", i, err)
			os.Exit(1)
		}
	}

	fmt.Printf("Successfully restored %d rules\n", len(rules))
}
