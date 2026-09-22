// Command sign creates a signed .pkg file from a binary and metadata.
package main

import (
	"crypto"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"os"

	"phaethon/pkg/signing"
)

func main() {
	binaryPath := flag.String("binary", "", "Path to binary file")
	metaPath := flag.String("meta", "", "Path to meta.json file")
	keyPath := flag.String("key", "", "Path to private key (PEM)")
	outputPath := flag.String("output", "", "Output .pkg path")
	flag.Parse()

	if *binaryPath == "" || *metaPath == "" || *keyPath == "" || *outputPath == "" {
		fmt.Fprintln(os.Stderr, "Usage: sign -binary <path> -meta <path> -key <path> -output <path>")
		os.Exit(1)
	}

	// Read binary
	binary, err := os.ReadFile(*binaryPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading binary: %v\n", err)
		os.Exit(1)
	}

	// Read meta
	metaData, err := os.ReadFile(*metaPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading meta: %v\n", err)
		os.Exit(1)
	}
	var meta signing.PackageMeta
	if err := json.Unmarshal(metaData, &meta); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing meta.json: %v\n", err)
		os.Exit(1)
	}

	// Re-marshal meta to match what CreatePkg will write to zip
	// (signature must be over the exact bytes in the zip)
	metaJSON, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error marshaling meta: %v\n", err)
		os.Exit(1)
	}

	// Read private key
	keyData, err := os.ReadFile(*keyPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading key: %v\n", err)
		os.Exit(1)
	}
	privateKey, err := parsePrivateKey(keyData)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing private key: %v\n", err)
		os.Exit(1)
	}

	// Sign: sha256(binary + metaJSON) - must match what's in the zip
	hasher := sha256.New()
	hasher.Write(binary)
	hasher.Write(metaJSON)
	hash := hasher.Sum(nil)

	signature, err := privateKey.Sign(nil, hash, crypto.Hash(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error signing: %v\n", err)
		os.Exit(1)
	}

	// Create pkg
	if err := signing.CreatePkg(*outputPath, binary, meta, signature); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating pkg: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Created signed package: %s\n", *outputPath)
	fmt.Printf("  Version:  %s\n", meta.Version)
	fmt.Printf("  Platform: %s\n", meta.Platform)
	fmt.Printf("  Arch:     %s\n", meta.Arch)
	fmt.Printf("  Sig:      %s...\n", base64.StdEncoding.EncodeToString(signature)[:32])
}

func parsePrivateKey(data []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		// Try as raw hex
		if len(data) == ed25519.SeedSize*2 {
			seed := make([]byte, ed25519.SeedSize)
			for i := 0; i < ed25519.SeedSize; i++ {
				fmt.Sscanf(string(data[i*2:i*2+2]), "%02x", &seed[i])
			}
			return ed25519.NewKeyFromSeed(seed), nil
		}
		return nil, fmt.Errorf("failed to decode PEM block")
	}

	key, err := parsePKCS8(block.Bytes)
	if err != nil {
		return nil, err
	}
	return key, nil
}

func parsePKCS8(data []byte) (ed25519.PrivateKey, error) {
	// PKCS8 for Ed25519 has a specific structure
	// For simplicity, we extract the seed from the expected position
	if len(data) < 48 {
		return nil, fmt.Errorf("invalid PKCS8 data")
	}
	// The seed is at the end of the PKCS8 structure for Ed25519
	seed := data[len(data)-ed25519.SeedSize:]
	return ed25519.NewKeyFromSeed(seed), nil
}
