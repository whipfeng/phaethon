package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"

	bolt "go.etcd.io/bbolt"
	"phaethon/db"
)

// rulesKey 是聚合格式的单一 key（JSON 数组），主应用已不携带此格式，
// 由本工具自给自足。
var rulesKey = []byte("__all_rules__")

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: %s <db-path>\n", os.Args[0])
		os.Exit(1)
	}

	dbPath := os.Args[1]

	if err := db.Init(dbPath); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to init database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	fmt.Println("Migrating rules to new format...")

	err := db.Update(func(tx *bolt.Tx) error {
		rulesBucket := tx.Bucket(db.BucketRules)
		if rulesBucket == nil {
			fmt.Println("✓ Rules bucket not found, skipping migration")
			return nil
		}

		// Check if new format already exists
		existingData := rulesBucket.Get(rulesKey)
		if existingData != nil {
			var existingRules []string
			if err := json.Unmarshal(existingData, &existingRules); err == nil {
				fmt.Printf("✓ New format already exists with %d rules, skipping migration\n", len(existingRules))
				return nil
			}
		}

		// Read all individual rules (old format)
		rulesMap := make(map[string]string)
		err := rulesBucket.ForEach(func(k, v []byte) error {
			key := string(k)
			// Skip the new format key
			if key == string(rulesKey) {
				return nil
			}
			rulesMap[key] = string(v)
			return nil
		})
		if err != nil {
			return fmt.Errorf("failed to read rules: %w", err)
		}

		if len(rulesMap) == 0 {
			fmt.Println("✓ No old format rules found, skipping migration")
			return nil
		}

		fmt.Printf("Found %d individual rules in old format\n", len(rulesMap))

		// Sort keys to maintain order
		keys := make([]string, 0, len(rulesMap))
		for k := range rulesMap {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		// Build rules array
		rules := make([]string, 0, len(keys))
		for _, k := range keys {
			v := rulesMap[k]
			// Remove quotes if present (stored as JSON strings)
			var rule string
			if err := json.Unmarshal([]byte(v), &rule); err != nil {
				// If unmarshal fails, use raw value
				rule = v
			}
			rules = append(rules, rule)
		}

		fmt.Printf("Migrating %d rules to new format\n", len(rules))

		// Write all rules as single JSON array
		jsonData, err := json.Marshal(rules)
		if err != nil {
			return fmt.Errorf("failed to marshal rules: %w", err)
		}

		if err := rulesBucket.Put(rulesKey, jsonData); err != nil {
			return fmt.Errorf("failed to write new format rules: %w", err)
		}

		// Delete individual rule keys
		for _, k := range keys {
			if err := rulesBucket.Delete([]byte(k)); err != nil {
				return fmt.Errorf("failed to delete old rule key %s: %w", k, err)
			}
		}

		fmt.Printf("✓ Successfully migrated %d rules to new format\n", len(rules))
		fmt.Println("✓ Old individual rule keys deleted")

		return nil
	})

	if err != nil {
		fmt.Fprintf(os.Stderr, "Migration failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("\n✓ Rules migration completed successfully!")
}
