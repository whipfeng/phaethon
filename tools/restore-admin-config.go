package main

import (
	"encoding/json"
	"fmt"
	"os"

	"phaethon/config"
	"phaethon/db"
)

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

	adminConfig := &config.AdminConfig{
		Enabled:     true,
		Addr:        "0.0.0.0:39999",
		Token:       "",
		AuthEnabled: true,
		Username:    "admin",
		Password:    "layer4-admin-2026",
		TLSCert:     "/home/layer4/layer4-deploy/data/certs/admin.crt",
		TLSKey:      "/home/layer4/layer4-deploy/data/certs/admin.key",
	}

	if err := db.PutAdminConfig(adminConfig); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to save admin config: %v\n", err)
		os.Exit(1)
	}

	data, _ := json.MarshalIndent(adminConfig, "", "  ")
	fmt.Printf("✓ Admin config saved:\n%s\n", string(data))
}
