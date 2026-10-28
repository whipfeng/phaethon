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

	mappings := []*config.Mapping{
		{
			Name:     "htunnel-32457",
			Type:     "h_tunnel",
			Port:     32457,
			Password: "3245930005",
		},
		{
			Name: "socks5-test",
			Type: "socks5",
			Port: 1080,
		},
	}

	for _, m := range mappings {
		if err := db.PutMapping(m); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to save mapping %s: %v\n", m.Name, err)
			os.Exit(1)
		}
		data, _ := json.MarshalIndent(m, "", "  ")
		fmt.Printf("✓ Mapping saved: %s\n", string(data))
	}

	fmt.Println("\n✓ All mappings restored successfully")
}
