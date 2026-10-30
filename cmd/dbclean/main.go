// One-time maintenance tool: drop the retired proxy-health bucket from an
// existing phaethon bbolt database. Run while phaethon is STOPPED.
//
// Usage: go run ./cmd/dbclean <path-to-phaethon.db>
package main

import (
	"fmt"
	"os"

	bolt "go.etcd.io/bbolt"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Println("usage: dbclean <db-path>")
		os.Exit(1)
	}
	path := os.Args[1]

	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: 2 * 1e9})
	if err != nil {
		fmt.Println("open fail (is phaethon running?):", err)
		os.Exit(1)
	}
	defer db.Close()

	err = db.Update(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte("proxy-health")) == nil {
			fmt.Println("proxy-health bucket not present, nothing to do")
			return nil
		}
		if err := tx.DeleteBucket([]byte("proxy-health")); err != nil {
			return err
		}
		fmt.Println("proxy-health bucket dropped")
		return nil
	})
	if err != nil {
		fmt.Println("cleanup fail:", err)
		os.Exit(1)
	}
}
