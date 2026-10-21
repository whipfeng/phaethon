package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"strconv"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Phaethon Admin Signature Tool")
		fmt.Println("Usage:")
		fmt.Println("  phaethon-sign keygen                    - Generate ECDSA key pair")
		fmt.Println("  phaethon-sign sign <method> <path> <private-key-file> [body-file] - Sign a request")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "keygen":
		generateKeyPair()
	case "sign":
		if len(os.Args) < 5 {
			fmt.Fprintf(os.Stderr, "Usage: phaethon-sign sign <method> <path> <private-key-file> [body-file]\n")
			os.Exit(1)
		}
		method := os.Args[2]
		path := os.Args[3]
		keyFile := os.Args[4]
		bodyFile := ""
		if len(os.Args) >= 6 {
			bodyFile = os.Args[5]
		}
		signRequest(method, path, keyFile, bodyFile)
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", os.Args[1])
		os.Exit(1)
	}
}

func generateKeyPair() {
	// Generate ECDSA P-256 key pair
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to generate key: %v\n", err)
		os.Exit(1)
	}

	// Encode private key
	privBytes, err := x509.MarshalECPrivateKey(privateKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to marshal private key: %v\n", err)
		os.Exit(1)
	}
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privBytes})

	// Encode public key
	pubBytes, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to marshal public key: %v\n", err)
		os.Exit(1)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubBytes})

	// Write to files
	if err := os.WriteFile("admin.key", privPEM, 0600); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to write private key: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile("admin.pub", pubPEM, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to write public key: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("✓ Generated ECDSA P-256 key pair:")
	fmt.Println("  Private key: admin.key (keep this secret!)")
	fmt.Println("  Public key:  admin.pub (add this to admin config)")
	fmt.Println()
	fmt.Println("Add the public key to your admin config:")
	fmt.Println("  admin:")
	fmt.Println("    public-key: |")
	for _, line := range splitLines(string(pubPEM)) {
		fmt.Printf("      %s\n", line)
	}
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			line := s[start:i]
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			if len(line) > 0 {
				lines = append(lines, line)
			}
			start = i + 1
		}
	}
	if start < len(s) {
		line := s[start:]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		if len(line) > 0 {
			lines = append(lines, line)
		}
	}
	return lines
}

func signRequest(method, path, keyFile, bodyFile string) {
	// Read private key
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read private key: %v\n", err)
		os.Exit(1)
	}

	block, _ := pem.Decode(keyPEM)
	if block == nil {
		fmt.Fprintf(os.Stderr, "Failed to parse private key\n")
		os.Exit(1)
	}

	privateKey, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to parse private key: %v\n", err)
		os.Exit(1)
	}

	// Read body
	var bodyBytes []byte
	if bodyFile != "" {
		bodyBytes, err = os.ReadFile(bodyFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to read body file: %v\n", err)
			os.Exit(1)
		}
	}

	// Compute body hash
	bodyHash := sha256.Sum256(bodyBytes)

	// Generate timestamp
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)

	// Construct signed data
	signedData := fmt.Sprintf("%s\n%s\n%s\n%s",
		timestamp,
		method,
		path,
		hex.EncodeToString(bodyHash[:]),
	)

	// Sign
	hash := sha256.Sum256([]byte(signedData))
	sig, err := ecdsa.SignASN1(rand.Reader, privateKey, hash[:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to sign: %v\n", err)
		os.Exit(1)
	}

	// Output headers
	fmt.Printf("X-Timestamp: %s\n", timestamp)
	fmt.Printf("X-Signature: %s\n", hex.EncodeToString(sig))
	fmt.Println()
	fmt.Printf("Signed data:\n%s\n", signedData)
}
