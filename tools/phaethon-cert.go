package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage:")
		fmt.Println("  phaethon-cert ca                          - Generate CA certificate")
		fmt.Println("  phaethon-cert client <ca-key> <ca-cert>   - Generate client certificate")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "ca":
		generateCA()
	case "client":
		if len(os.Args) != 4 {
			fmt.Println("Usage: phaethon-cert client <ca-key> <ca-cert>")
			os.Exit(1)
		}
		generateClientCert(os.Args[2], os.Args[3])
	default:
		fmt.Printf("Unknown command: %s\n", os.Args[1])
		os.Exit(1)
	}
}

func generateCA() {
	// Generate ECDSA P-256 private key
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to generate CA key: %v\n", err)
		os.Exit(1)
	}

	// Create CA certificate template
	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to generate serial number: %v\n", err)
		os.Exit(1)
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"Phaethon Admin"},
			CommonName:   "Phaethon Admin CA",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour), // 10 years
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            1,
	}

	// Self-sign the CA certificate
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create CA certificate: %v\n", err)
		os.Exit(1)
	}

	// Write CA private key
	keyFile, err := os.Create("ca.key")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create ca.key: %v\n", err)
		os.Exit(1)
	}
	defer keyFile.Close()

	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to marshal CA key: %v\n", err)
		os.Exit(1)
	}

	if err := pem.Encode(keyFile, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to write CA key: %v\n", err)
		os.Exit(1)
	}

	// Write CA certificate
	certFile, err := os.Create("ca.crt")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create ca.crt: %v\n", err)
		os.Exit(1)
	}
	defer certFile.Close()

	if err := pem.Encode(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to write CA certificate: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("✓ Generated CA certificate:")
	fmt.Println("  CA private key: ca.key (keep this secret!)")
	fmt.Println("  CA certificate: ca.crt (add this to admin config as client-ca)")
	fmt.Println("")
	fmt.Println("Add the CA certificate to your admin config:")
	fmt.Println("  admin:")
	fmt.Println("    client-ca: |")
	fmt.Println("      -----BEGIN CERTIFICATE-----")
	fmt.Println("      ... (contents of ca.crt) ...")
	fmt.Println("      -----END CERTIFICATE-----")
}

func generateClientCert(caKeyPath, caCertPath string) {
	// Read CA private key
	caKeyPEM, err := os.ReadFile(caKeyPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read CA key: %v\n", err)
		os.Exit(1)
	}

	caKeyBlock, _ := pem.Decode(caKeyPEM)
	if caKeyBlock == nil {
		fmt.Fprintf(os.Stderr, "Failed to parse CA key PEM\n")
		os.Exit(1)
	}

	caKey, err := x509.ParseECPrivateKey(caKeyBlock.Bytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to parse CA key: %v\n", err)
		os.Exit(1)
	}

	// Read CA certificate
	caCertPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read CA cert: %v\n", err)
		os.Exit(1)
	}

	caCertBlock, _ := pem.Decode(caCertPEM)
	if caCertBlock == nil {
		fmt.Fprintf(os.Stderr, "Failed to parse CA cert PEM\n")
		os.Exit(1)
	}

	caCert, err := x509.ParseCertificate(caCertBlock.Bytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to parse CA cert: %v\n", err)
		os.Exit(1)
	}

	// Generate client private key
	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to generate client key: %v\n", err)
		os.Exit(1)
	}

	// Create client certificate template
	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to generate serial number: %v\n", err)
		os.Exit(1)
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"Phaethon Admin"},
			CommonName:   "Phaethon Admin Client",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(5 * 365 * 24 * time.Hour), // 5 years
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}

	// Sign client certificate with CA
	certDER, err := x509.CreateCertificate(rand.Reader, &template, caCert, &clientKey.PublicKey, caKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create client certificate: %v\n", err)
		os.Exit(1)
	}

	// Write client private key
	keyFile, err := os.Create("client.key")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create client.key: %v\n", err)
		os.Exit(1)
	}
	defer keyFile.Close()

	keyBytes, err := x509.MarshalECPrivateKey(clientKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to marshal client key: %v\n", err)
		os.Exit(1)
	}

	if err := pem.Encode(keyFile, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to write client key: %v\n", err)
		os.Exit(1)
	}

	// Write client certificate
	certFile, err := os.Create("client.crt")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create client.crt: %v\n", err)
		os.Exit(1)
	}
	defer certFile.Close()

	if err := pem.Encode(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to write client certificate: %v\n", err)
		os.Exit(1)
	}

	// Create PKCS#12 bundle for browser import (optional)
	fmt.Println("✓ Generated client certificate:")
	fmt.Println("  Client private key: client.key (install in browser)")
	fmt.Println("  Client certificate: client.crt (install in browser)")
	fmt.Println("")
	fmt.Println("To use in browser:")
	fmt.Println("  1. Convert to PKCS#12 (optional):")
	fmt.Println("     openssl pkcs12 -export -out client.p12 -inkey client.key -in client.crt")
	fmt.Println("  2. Import client.p12 (or client.crt + client.key) into browser certificate store")
	fmt.Println("  3. Access admin panel with HTTPS - browser will present client certificate")
}
