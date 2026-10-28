package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"phaethon/config"
	"phaethon/db"
	"phaethon/util"
)

func main() {
	dataDir := flag.String("data-dir", "./data", "Data directory")
	nodeID := flag.String("node-id", "", "Node ID")
	subnet := flag.String("subnet", "", "Subnet (e.g., 100.2.0.0/16)")
	network := flag.String("network", "100.0.0.0/8", "Network (default: 100.0.0.0/8)")
	flag.Parse()

	if *nodeID == "" || *subnet == "" {
		fmt.Println("Usage: set-mesh-config --node-id <id> --subnet <subnet> [--network <network>] [--data-dir <dir>]")
		flag.PrintDefaults()
		os.Exit(1)
	}

	// Initialize database
	dbPath := filepath.Join(*dataDir, "phaethon.db")
	if err := db.Init(dbPath); err != nil {
		util.LogError("Failed to init database: %v", err)
		os.Exit(1)
	}
	defer db.Close()

	// Create mesh config
	meshConfig := &config.MeshConfig{
		NodeID:  *nodeID,
		Subnet:  *subnet,
		Network: *network,
	}

	// Save to database
	if err := db.PutMeshConfig(meshConfig); err != nil {
		util.LogError("Failed to save mesh config: %v", err)
		os.Exit(1)
	}

	fmt.Printf("✅ Mesh config saved: nodeID=%s, subnet=%s, network=%s\n", *nodeID, *subnet, *network)

	// Verify
	loaded, err := db.GetMeshConfig()
	if err != nil {
		util.LogError("Failed to verify: %v", err)
		os.Exit(1)
	}
	data, _ := json.MarshalIndent(loaded, "", "  ")
	fmt.Printf("Verified: %s\n", data)
}
