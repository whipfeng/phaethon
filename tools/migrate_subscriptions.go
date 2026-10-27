// migrate_subscriptions.go - One-time migration tool
// Purpose: Populate subscription nodes after restoring from old database backup
//
// Background:
// - Before Oct 23 14:58, subscription nodes were only in memory (sub.SubProxies)
// - After Oct 23 14:58, nodes are persisted in BucketSubscriptionNodes
// - Old backups don't have node data in the new format
//
// Usage:
//   go run tools/migrate_subscriptions.go /path/to/data/dir
//
// This tool:
// 1. Opens the database
// 2. Lists all subscriptions
// 3. For each subscription missing nodes, fetches from URL
// 4. Stores nodes in BucketSubscriptionNodes
// 5. Exits

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"phaethon/config"
	"phaethon/db"
	"phaethon/util"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Printf("Usage: %s <data-dir>\n", os.Args[0])
		fmt.Printf("Example: %s /root/data\n", os.Args[0])
		os.Exit(1)
	}

	dataDir := os.Args[1]
	fmt.Printf("Starting subscription migration...\n")
	fmt.Printf("Data directory: %s\n\n", dataDir)

	// Initialize database
	if err := db.Init(dataDir); err != nil {
		fmt.Printf("ERROR: Failed to initialize database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	subCacheDir := filepath.Join(dataDir, "cache", "subscriptions")

	// Get all subscriptions
	subs, err := db.ListSubscriptions()
	if err != nil {
		fmt.Printf("ERROR: Failed to list subscriptions: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Found %d subscriptions\n\n", len(subs))

	migrated := 0
	skipped := 0
	failed := 0

	for _, sub := range subs {
		if !sub.IsEnabled() || sub.URL == "" {
			fmt.Printf("[SKIP] %s: disabled or no URL\n", sub.Name)
			skipped++
			continue
		}

		// Check if nodes already exist
		nodes, err := db.GetSubscriptionNodes(sub.Name)
		if err == nil && len(nodes) > 0 {
			fmt.Printf("[SKIP] %s: already has %d nodes\n", sub.Name, len(nodes))
			skipped++
			continue
		}

		fmt.Printf("[MIGRATE] %s: fetching from %s...\n", sub.Name, sub.URL)

		// Fetch subscription content
		content, err := config.FetchSubscription(sub.URL, nil)
		if err != nil {
			fmt.Printf("[FAIL] %s: fetch failed: %v\n", sub.Name, err)
			failed++
			continue
		}

		// Parse subscription
		proxies, err := config.ParseSubscription(content)
		if err != nil {
			fmt.Printf("[FAIL] %s: parse failed: %v\n", sub.Name, err)
			failed++
			continue
		}

		if len(proxies) == 0 {
			fmt.Printf("[FAIL] %s: no nodes found\n", sub.Name)
			failed++
			continue
		}

		// Save nodes to database
		if err := db.PutSubscriptionNodes(sub.Name, proxies); err != nil {
			fmt.Printf("[FAIL] %s: save failed: %v\n", sub.Name, err)
			failed++
			continue
		}

		fmt.Printf("[OK] %s: migrated %d nodes\n", sub.Name, len(proxies))
		migrated++
	}

	fmt.Printf("\n========================================\n")
	fmt.Printf("Migration complete:\n")
	fmt.Printf("  Migrated: %d\n", migrated)
	fmt.Printf("  Skipped:  %d\n", skipped)
	fmt.Printf("  Failed:   %d\n", failed)
	fmt.Printf("========================================\n")

	if failed > 0 {
		os.Exit(1)
	}
}
