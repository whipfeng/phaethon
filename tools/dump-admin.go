package main

import (
	"fmt"
	"os"
	bolt "go.etcd.io/bbolt"
)

func main() {
	db, _ := bolt.Open(os.Args[1], 0600, nil)
	defer db.Close()
	db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("admin-config"))
		if b == nil { return nil }
		v := b.Get([]byte("admin"))
		fmt.Printf("%s\n", string(v))
		return nil
	})
}
