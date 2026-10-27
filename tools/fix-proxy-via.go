package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"

	"phaethon/config"
	"phaethon/db"

	bolt "go.etcd.io/bbolt"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("Usage: fix-proxy-via <db-path>")
	}
	dbPath := os.Args[1]

	boltDB, err := bolt.Open(dbPath, 0600, nil)
	if err != nil {
		log.Fatalf("Open database fail: %v", err)
	}
	defer boltDB.Close()

	db.InitWithDB(boltDB)

	proxy, err := db.GetProxy("GGDYSV_TJ")
	if err != nil {
		log.Fatalf("Get proxy fail: %v", err)
	}
	if proxy == nil {
		log.Fatal("Proxy GGDYSV_TJ not found")
	}

	fmt.Printf("Before: name=%s type=%s server=%s via=%s\n", proxy.Name, proxy.Type, proxy.Server, proxy.ViaProxy)

	proxy.ViaProxy = "MGMS_HT"

	if err := db.PutProxy(proxy); err != nil {
		log.Fatalf("Put proxy fail: %v", err)
	}

	fmt.Printf("After:  name=%s type=%s server=%s via=%s\n", proxy.Name, proxy.Type, proxy.Server, proxy.ViaProxy)
	fmt.Println("Done! Restart phaethon to apply changes.")
}
