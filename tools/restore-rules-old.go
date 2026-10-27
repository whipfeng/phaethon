package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"phaethon/db"

	"go.etcd.io/bbolt"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: restore-rules-old <backup-db-path>")
		os.Exit(1)
	}

	backupPath := os.Args[1]
	dbPath := "/root/data/phaethon.db"

	// Initialize current database
	if err := db.Init(dbPath); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to init database: %v\n", err)
		os.Exit(1)
	}

	// Open backup database
	backupDB, err := bbolt.Open(backupPath, 0600, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open backup: %v\n", err)
		os.Exit(1)
	}
	defer backupDB.Close()

	// Read rules from backup (old format: individual keys)
	rulesMap := make(map[string]string)
	err = backupDB.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("rules"))
		if b == nil {
			return fmt.Errorf("rules bucket not found")
		}
		return b.ForEach(func(k, v []byte) error {
			key := string(k)
			// Skip metadata keys
			if strings.HasPrefix(key, "__") || key == "rebuilt" {
				return nil
			}
			rulesMap[key] = string(v)
			return nil
		})
	})

	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read rules from backup: %v\n", err)
		os.Exit(1)
	}

	// Sort keys to maintain order
	keys := make([]string, 0, len(rulesMap))
	for k := range rulesMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	fmt.Printf("Found %d rules in backup (old format)\n", len(keys))

	// Extract rule strings (values are JSON-encoded with quotes)
	var rules []string
	for _, k := range keys {
		v := rulesMap[k]
		// Remove surrounding quotes if present
		v = strings.Trim(v, `"`)
		rules = append(rules, v)
	}

	// Write all rules at once using PutRules (new format)
	if err := db.PutRules(rules); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to write rules: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Successfully restored %d rules (new format)\n", len(rules))
}
