package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"

	bolt "go.etcd.io/bbolt"
	"phaethon/config"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: enable-admin <db-path>")
		os.Exit(1)
	}

	dbPath := os.Args[1]
	db, err := bolt.Open(dbPath, 0600, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	err = db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("config"))
		if b == nil {
			return fmt.Errorf("bucket 'config' not found")
		}

		data := b.Get([]byte("admin"))
		if data == nil {
			fmt.Println("Admin config not found, creating new one")
			admin := &config.AdminConfig{
				Enabled:     true,
				Addr:        ":39999",
				AuthEnabled: false,
			}
			jsonData, err := json.Marshal(admin)
			if err != nil {
				return err
			}
			return b.Put([]byte("admin"), jsonData)
		}

		var admin config.AdminConfig
		if err := json.Unmarshal(data, &admin); err != nil {
			return err
		}

		fmt.Printf("Before: Enabled=%v, Addr=%s, AuthEnabled=%v\n", admin.Enabled, admin.Addr, admin.AuthEnabled)

		admin.Enabled = true
		admin.AuthEnabled = false

		jsonData, err := json.Marshal(admin)
		if err != nil {
			return err
		}

		if err := b.Put([]byte("admin"), jsonData); err != nil {
			return err
		}

		fmt.Printf("After: Enabled=%v, Addr=%s, AuthEnabled=%v\n", admin.Enabled, admin.Addr, admin.AuthEnabled)
		return nil
	})

	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("Admin config updated successfully")
}
