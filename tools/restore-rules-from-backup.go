package main

import (
	"encoding/json"
	"fmt"
	"os"
	"phaethon/db"
	bolt "go.etcd.io/bbolt"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: restore-rules-from-backup <backup-db-path>")
		os.Exit(1)
	}

	backupPath := os.Args[1]

	// Open backup database to read rules
	backupDB, err := bolt.Open(backupPath, 0600, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open backup: %v\n", err)
		os.Exit(1)
	}
	defer backupDB.Close()

	// Read all rules from backup (they're stored as individual keys with zero-padded string IDs like "000000")
	var rules []string
	err = backupDB.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("rules"))
		if b == nil {
			return fmt.Errorf("rules bucket not found in backup")
		}

		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			key := string(k)
			// Skip metadata keys
			if key == "__order__" || key == "__next_id__" || key == "__all_rules__" {
				continue
			}
			// This is a rule entry - value is JSON-encoded string
			if v != nil {
				var rule string
				if err := json.Unmarshal(v, &rule); err != nil {
					// If not JSON, use as-is
					rule = string(v)
				}
				rules = append(rules, rule)
			}
		}
		return nil
	})

	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read rules from backup: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Found %d rules in backup\n", len(rules))

	if len(rules) == 0 {
		fmt.Println("No rules to restore")
		return
	}

	// Initialize current database
	if err := db.Init("data/phaethon.db"); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to init current DB: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	// Clear current rules
	err = db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(db.BucketRules))
		if b == nil {
			return fmt.Errorf("rules bucket not found in current DB")
		}

		// Delete all rule entries (keep metadata for now)
		c := b.Cursor()
		var keysToDelete [][]byte
		for k, _ := c.First(); k != nil; k, _ = c.Next() {
			key := string(k)
			if key != "__order__" && key != "__next_id__" && key != "__all_rules__" {
				keysToDelete = append(keysToDelete, k)
			}
		}
		for _, k := range keysToDelete {
			if err := b.Delete(k); err != nil {
				return err
			}
		}

		// Reset order and next_id
		if err := b.Put([]byte("__order__"), []byte("[]")); err != nil {
			return err
		}
		return b.Put([]byte("__next_id__"), []byte("1"))
	})

	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to clear current rules: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Cleared current rules")

	// Insert rules from backup using the new stable ID system
	for i, rule := range rules {
		_, err := db.InsertRuleWithID(rule, nil) // nil = append to end
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to insert rule %d: %v\n", i, err)
			os.Exit(1)
		}
	}

	fmt.Printf("Successfully restored %d rules\n", len(rules))
}
