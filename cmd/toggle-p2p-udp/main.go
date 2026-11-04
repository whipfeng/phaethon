// Command toggle-p2p-udp flips the p2p-udp flag of a named proxy in a
// phaethon bolt database. The worker must be stopped first (bolt single-writer).
//
// Usage: toggle-p2p-udp.exe -db <path-to-phaethon.db> -name <proxy> [-on|-off]
package main

import (
	"flag"
	"fmt"
	"os"

	"phaethon/db"
)

func main() {
	dbPath := flag.String("db", "", "path to phaethon.db")
	name := flag.String("name", "", "proxy name")
	on := flag.Bool("on", false, "enable p2p-udp")
	off := flag.Bool("off", false, "disable p2p-udp")
	flag.Parse()

	if *dbPath == "" || *name == "" || *on == *off {
		fmt.Fprintln(os.Stderr, "usage: toggle-p2p-udp -db <db> -name <proxy> (-on|-off)")
		os.Exit(2)
	}
	if err := db.Init(*dbPath); err != nil {
		fmt.Fprintln(os.Stderr, "db init:", err)
		os.Exit(1)
	}
	p, err := db.GetProxy(*name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "get proxy:", err)
		os.Exit(1)
	}
	if p == nil {
		fmt.Fprintln(os.Stderr, "proxy not found:", *name)
		os.Exit(1)
	}
	v := *on
	fmt.Printf("before: %s p2p-udp=%v\n", p.Name, p.IsP2PUDP())
	p.P2PUDP = &v
	if err := db.PutProxy(p); err != nil {
		fmt.Fprintln(os.Stderr, "put proxy:", err)
		os.Exit(1)
	}
	p2, _ := db.GetProxy(*name)
	fmt.Printf("after:  %s p2p-udp=%v\n", p2.Name, p2.IsP2PUDP())
}
