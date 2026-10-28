package main

import (
	"encoding/json"
	"fmt"
	"os"

	bolt "go.etcd.io/bbolt"
)

type AdminConfig struct {
	Enabled            bool   `json:"Enabled"`
	Addr               string `json:"Addr"`
	Token              string `json:"Token"`
	AuthEnabled        bool   `json:"AuthEnabled"`
	Username           string `json:"Username"`
	Password           string `json:"Password"`
	TLSCert            string `json:"TLSCert"`
	TLSKey             string `json:"TLSKey"`
	MeshOnly           bool   `json:"MeshOnly"`
	PackageUploadMaxMB int    `json:"PackageUploadMaxMB"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: %s <db-path>\n", os.Args[0])
		os.Exit(1)
	}

	dbPath := os.Args[1]

	db, err := bolt.Open(dbPath, 0600, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open db: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	err = db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("admin-config"))
		if b == nil {
			return fmt.Errorf("admin-config bucket not found")
		}

		data := b.Get([]byte("admin"))
		if data == nil {
			return fmt.Errorf("admin key not found")
		}

		var cfg AdminConfig
		if err := json.Unmarshal(data, &cfg); err != nil {
			return fmt.Errorf("failed to unmarshal: %w", err)
		}

		fmt.Printf("Before: TLSCert=%q, TLSKey=%q\n", cfg.TLSCert, cfg.TLSKey)

		cfg.TLSCert = "admin-tls-cert.pem"
		cfg.TLSKey = "admin-tls-key.pem"

		newData, err := json.Marshal(cfg)
		if err != nil {
			return fmt.Errorf("failed to marshal: %w", err)
		}

		if err := b.Put([]byte("admin"), newData); err != nil {
			return fmt.Errorf("failed to update: %w", err)
		}

		fmt.Printf("After: TLSCert=%q, TLSKey=%q\n", cfg.TLSCert, cfg.TLSKey)
		fmt.Println("✓ Admin config updated with TLS paths")
		return nil
	})

	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to update: %v\n", err)
		os.Exit(1)
	}
}
