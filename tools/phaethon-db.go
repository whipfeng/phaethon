package main

import (
	"encoding/json"
	"fmt"
	"os"

	bolt "go.etcd.io/bbolt"
)

type AdminConfig struct {
	AuthEnabled bool   `json:"authEnabled"`
	Password    string `json:"password"`
}

func main() {
	if len(os.Args) < 3 {
		fmt.Println("Phaethon Database Admin Tool")
		fmt.Println("Usage:")
		fmt.Println("  phaethon-db <db-path> get-admin     - Get admin config")
		fmt.Println("  phaethon-db <db-path> set-password <password> - Set admin password")
		os.Exit(1)
	}

	dbPath := os.Args[1]
	command := os.Args[2]

	db, err := bolt.Open(dbPath, 0600, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	switch command {
	case "get-admin":
		getAdminConfig(db)
	case "set-password":
		if len(os.Args) < 4 {
			fmt.Fprintf(os.Stderr, "Error: password argument required\n")
			os.Exit(1)
		}
		setPassword(db, os.Args[3])
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", command)
		os.Exit(1)
	}
}

func getAdminConfig(db *bolt.DB) {
	err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("config"))
		if b == nil {
			return fmt.Errorf("config bucket not found")
		}
		v := b.Get([]byte("admin_config"))
		if v == nil {
			return fmt.Errorf("admin_config not found")
		}
		fmt.Printf("Admin config: %s\n", string(v))
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func setPassword(db *bolt.DB, newPassword string) {
	err := db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("config"))
		if b == nil {
			return fmt.Errorf("config bucket not found")
		}

		v := b.Get([]byte("admin_config"))
		var config map[string]interface{}
		if v != nil {
			if err := json.Unmarshal(v, &config); err != nil {
				return fmt.Errorf("failed to parse admin_config: %v", err)
			}
		} else {
			config = make(map[string]interface{})
		}

		admin, ok := config["admin"].(map[string]interface{})
		if !ok {
			admin = make(map[string]interface{})
			config["admin"] = admin
		}

		admin["password"] = newPassword
		admin["authEnabled"] = true

		newValue, err := json.Marshal(config)
		if err != nil {
			return fmt.Errorf("failed to marshal config: %v", err)
		}

		if err := b.Put([]byte("admin_config"), newValue); err != nil {
			return fmt.Errorf("failed to save config: %v", err)
		}

		fmt.Printf("✓ Password set to: %s\n", newPassword)
		return nil
	})

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
