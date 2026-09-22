package signing

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// TestPublicKey is a hardcoded Ed25519 public key for testing.
// TODO: Replace with production key management.
// This is a test key pair (DO NOT USE IN PRODUCTION):
// Seed:    0x000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f
// Public:  0x03a107bff3ce10be1d70dd18e74bc09967e4d6309ba50d5f1ddc8664125531b8
var TestPublicKey = func() ed25519.PublicKey {
	// Generate a test key pair for development
	// In production, this should be loaded from config or compiled in
	pubHex := "03a107bff3ce10be1d70dd18e74bc09967e4d6309ba50d5f1ddc8664125531b8"
	pubBytes, _ := hex.DecodeString(pubHex)
	return ed25519.PublicKey(pubBytes)
}()

// PackageMeta represents the metadata in a .pkg file.
type PackageMeta struct {
	Version   string `json:"version"`
	Platform  string `json:"platform"`
	Arch      string `json:"arch"`
	BuildTime string `json:"buildTime,omitempty"`
	GitCommit string `json:"gitCommit,omitempty"`
	GoVersion string `json:"goVersion,omitempty"`
	BuildTag  string `json:"buildTag,omitempty"`
}

// PkgContents represents the extracted contents of a .pkg file.
type PkgContents struct {
	Binary    []byte
	Meta      PackageMeta
	Signature []byte
}

// VerifyPkg extracts and verifies a .pkg file.
// Returns the contents if signature is valid, error otherwise.
func VerifyPkg(pkgPath string, publicKey ed25519.PublicKey) (*PkgContents, error) {
	// Open zip file
	r, err := zip.OpenReader(pkgPath)
	if err != nil {
		return nil, fmt.Errorf("open pkg: %w", err)
	}
	defer r.Close()

	var binary, signature []byte
	var meta PackageMeta
	var metaRaw []byte

	// Extract files
	for _, f := range r.File {
		// Security: validate path strictly
		// 1. Clean the path
		cleanName := filepath.Clean(f.Name)
		// 2. Reject absolute paths
		if filepath.IsAbs(cleanName) {
			return nil, fmt.Errorf("invalid path in pkg: %s (absolute)", f.Name)
		}
		// 3. Reject paths that escape (.. or start with ..)
		if strings.HasPrefix(cleanName, "..") || strings.Contains(cleanName, string(filepath.Separator)+"..") {
			return nil, fmt.Errorf("invalid path in pkg: %s (path traversal)", f.Name)
		}
		// 4. Only allow specific filenames (whitelist)
		if cleanName != "binary" && cleanName != "meta.json" && cleanName != "signature" {
			return nil, fmt.Errorf("unexpected file in pkg: %s", f.Name)
		}

		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", f.Name, err)
		}

		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", f.Name, err)
		}

		switch cleanName {
		case "binary":
			binary = data
		case "meta.json":
			metaRaw = data
			if err := json.Unmarshal(data, &meta); err != nil {
				return nil, fmt.Errorf("parse meta.json: %w", err)
			}
		case "signature":
			sigStr := strings.TrimSpace(string(data))
			signature, err = base64.StdEncoding.DecodeString(sigStr)
			if err != nil {
				return nil, fmt.Errorf("decode signature: %w", err)
			}
		}
	}

	// Validate required files
	if binary == nil {
		return nil, fmt.Errorf("missing binary in pkg")
	}
	if metaRaw == nil {
		return nil, fmt.Errorf("missing meta.json in pkg")
	}
	if signature == nil {
		return nil, fmt.Errorf("missing signature in pkg")
	}

	// Verify signature: sha256(binary + meta.json)
	hasher := sha256.New()
	hasher.Write(binary)
	hasher.Write(metaRaw)
	hash := hasher.Sum(nil)

	if !ed25519.Verify(publicKey, hash, signature) {
		return nil, fmt.Errorf("signature verification failed")
	}

	return &PkgContents{
		Binary:    binary,
		Meta:      meta,
		Signature: signature,
	}, nil
}

// CreatePkg creates a .pkg file from binary, metadata, and signature.
// Used for testing and build tools.
func CreatePkg(pkgPath string, binary []byte, meta PackageMeta, signature []byte) error {
	f, err := os.Create(pkgPath)
	if err != nil {
		return err
	}
	defer f.Close()

	w := zip.NewWriter(f)
	defer w.Close()

	// Write binary
	bw, err := w.Create("binary")
	if err != nil {
		return err
	}
	if _, err := bw.Write(binary); err != nil {
		return err
	}

	// Write meta.json
	metaJSON, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	mw, err := w.Create("meta.json")
	if err != nil {
		return err
	}
	if _, err := mw.Write(metaJSON); err != nil {
		return err
	}

	// Write signature (base64 encoded)
	sigB64 := base64.StdEncoding.EncodeToString(signature)
	sw, err := w.Create("signature")
	if err != nil {
		return err
	}
	if _, err := sw.Write([]byte(sigB64)); err != nil {
		return err
	}

	return nil
}

// SignData signs binary + meta.json content with the given private key.
// Returns the signature.
func SignData(binary, metaJSON []byte, privateKey ed25519.PrivateKey) []byte {
	hasher := sha256.New()
	hasher.Write(binary)
	hasher.Write(metaJSON)
	hash := hasher.Sum(nil)
	return ed25519.Sign(privateKey, hash)
}

// ComputeHash computes sha256 of binary + meta.json (for debugging/display).
func ComputeHash(binary, metaJSON []byte) string {
	hasher := sha256.New()
	hasher.Write(binary)
	hasher.Write(metaJSON)
	return hex.EncodeToString(hasher.Sum(nil))
}

// GenerateTestKeyPair generates a test Ed25519 key pair.
// DO NOT USE IN PRODUCTION.
func GenerateTestKeyPair() (ed25519.PublicKey, ed25519.PrivateKey) {
	// Use a deterministic seed for reproducible test keys
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	return publicKey, privateKey
}

// VerifyPkgBytes verifies a .pkg file from memory (not from disk).
func VerifyPkgBytes(pkgData []byte, publicKey ed25519.PublicKey) (*PkgContents, error) {
	r, err := zip.NewReader(bytes.NewReader(pkgData), int64(len(pkgData)))
	if err != nil {
		return nil, fmt.Errorf("open pkg: %w", err)
	}

	var binary, signature []byte
	var meta PackageMeta
	var metaRaw []byte

	for _, f := range r.File {
		// Security: validate path strictly
		cleanName := filepath.Clean(f.Name)
		if filepath.IsAbs(cleanName) {
			return nil, fmt.Errorf("invalid path in pkg: %s (absolute)", f.Name)
		}
		if strings.HasPrefix(cleanName, "..") || strings.Contains(cleanName, string(filepath.Separator)+"..") {
			return nil, fmt.Errorf("invalid path in pkg: %s (path traversal)", f.Name)
		}
		if cleanName != "binary" && cleanName != "meta.json" && cleanName != "signature" {
			return nil, fmt.Errorf("unexpected file in pkg: %s", f.Name)
		}

		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", f.Name, err)
		}

		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", f.Name, err)
		}

		switch cleanName {
		case "binary":
			binary = data
		case "meta.json":
			metaRaw = data
			if err := json.Unmarshal(data, &meta); err != nil {
				return nil, fmt.Errorf("parse meta.json: %w", err)
			}
		case "signature":
			sigStr := strings.TrimSpace(string(data))
			signature, err = base64.StdEncoding.DecodeString(sigStr)
			if err != nil {
				return nil, fmt.Errorf("decode signature: %w", err)
			}
		}
	}

	if binary == nil {
		return nil, fmt.Errorf("missing binary in pkg")
	}
	if metaRaw == nil {
		return nil, fmt.Errorf("missing meta.json in pkg")
	}
	if signature == nil {
		return nil, fmt.Errorf("missing signature in pkg")
	}

	hasher := sha256.New()
	hasher.Write(binary)
	hasher.Write(metaRaw)
	hash := hasher.Sum(nil)

	if !ed25519.Verify(publicKey, hash, signature) {
		return nil, fmt.Errorf("signature verification failed")
	}

	return &PkgContents{
		Binary:    binary,
		Meta:      meta,
		Signature: signature,
	}, nil
}
