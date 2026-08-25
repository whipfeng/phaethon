package main

import (
	"fmt"
	"golang.org/x/crypto/chacha20poly1305"
)

func main() {
	key := make([]byte, 32)
	aead, _ := chacha20poly1305.NewX(key)
	fmt.Println("NonceSize:", aead.NonceSize())
	fmt.Println("Overhead:", aead.Overhead())
}
