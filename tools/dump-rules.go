package main

import (
	"encoding/json"
	"fmt"
	"os"
	bolt "go.etcd.io/bbolt"
)

func main() {
	db, _ := bolt.Open(os.Args[1], 0600, nil)
	defer db.Close()
	db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("rules"))
		if b == nil { return nil }
		v := b.Get([]byte("__all_rules__"))
		if v == nil { return nil }
		var rules []string
		json.Unmarshal(v, &rules)
		for i, r := range rules {
			if i < 20 || i >= len(rules)-5 {
				fmt.Printf("[%d] %s\n", i, r)
			} else if i == 20 {
				fmt.Println("...")
			}
		}
		fmt.Printf("\nTotal: %d rules\n", len(rules))
		return nil
	})
}
