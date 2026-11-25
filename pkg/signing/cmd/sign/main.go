// Command sign creates a signed .pkg file from a binary and metadata.
// The version is extracted from the binary by running it with --version.
// Platform and arch must be specified via flags or meta.json.
package main

import (
	"crypto"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"phaethon/pkg/signing"
)

func main() {
	binaryPath := flag.String("binary", "", "Path to binary file")
	metaPath := flag.String("meta", "", "Path to meta.json file (optional, version auto-extracted from binary)")
	keyPath := flag.String("key", "", "Path to private key (PEM)")
	outputPath := flag.String("output", "", "Output .pkg path")
	platform := flag.String("platform", "", "Platform (linux/windows/darwin) - required if no meta.json")
	arch := flag.String("arch", "", "Architecture (amd64/arm64) - required if no meta.json")
	versionFlag := flag.String("version", "", "Version override; skips running the binary (for cross-compiled targets)")
	var assetFlags assetFlagList
	flag.Var(&assetFlags, "asset", "Asset to ship as assets/<name>=<path> (repeatable, e.g. winpty.dll=third_party/winpty/amd64/winpty.dll)")
	flag.Parse()

	if *binaryPath == "" || *keyPath == "" || *outputPath == "" {
		fmt.Fprintln(os.Stderr, "Usage: sign -binary <path> -key <path> -output <path> [-meta <path>] [-platform <os>] [-arch <arch>] [-version <v>]")
		os.Exit(1)
	}

	// Read binary
	binary, err := os.ReadFile(*binaryPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading binary: %v\n", err)
		os.Exit(1)
	}

	// Extract version from binary by running --version. Cross-compiled targets
	// cannot run locally, so an explicit -version is accepted instead.
	version := *versionFlag
	if version == "" {
		version, err = extractVersion(*binaryPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error extracting version from binary: %v (use -version for cross-compiled targets)\n", err)
			os.Exit(1)
		}
	}
	fmt.Printf("Version: %s\n", version)

	var meta signing.PackageMeta

	if *metaPath != "" {
		// Read meta from file
		metaData, err := os.ReadFile(*metaPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading meta: %v\n", err)
			os.Exit(1)
		}
		if err := json.Unmarshal(metaData, &meta); err != nil {
			fmt.Fprintf(os.Stderr, "Error parsing meta.json: %v\n", err)
			os.Exit(1)
		}
		// Override version with the one extracted from binary
		meta.Version = version
	} else {
		// Auto-generate meta from binary
		if *platform == "" || *arch == "" {
			fmt.Fprintln(os.Stderr, "Error: -platform and -arch are required when no -meta is provided")
			os.Exit(1)
		}
		meta = signing.PackageMeta{
			Version:  version,
			Platform: *platform,
			Arch:     *arch,
		}
	}

	// Read assets, record their digests in meta, and keep the bytes for the zip.
	assets := make(map[string][]byte, len(assetFlags))
	meta.Assets = nil
	for _, spec := range assetFlags {
		name, path, ok := strings.Cut(spec, "=")
		if !ok || name == "" || path == "" {
			fmt.Fprintf(os.Stderr, "Error: invalid -asset %q (want name=path)\n", spec)
			os.Exit(1)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading asset %s: %v\n", path, err)
			os.Exit(1)
		}
		sum := sha256.Sum256(data)
		meta.Assets = append(meta.Assets, signing.AssetMeta{
			Name:   name,
			Size:   int64(len(data)),
			SHA256: hex.EncodeToString(sum[:]),
		})
		assets[name] = data
		fmt.Printf("Asset: %s (%d bytes)\n", name, len(data))
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
	if err := signing.CreatePkgWithAssets(*outputPath, binary, meta, signature, assets); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating pkg: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Created signed package: %s\n", *outputPath)
	fmt.Printf("  Version:  %s\n", meta.Version)
	fmt.Printf("  Platform: %s\n", meta.Platform)
	fmt.Printf("  Arch:     %s\n", meta.Arch)
	fmt.Printf("  Sig:      %s...\n", base64.StdEncoding.EncodeToString(signature)[:32])
}

// assetFlagList collects repeated -asset name=path flags in order.
type assetFlagList []string

func (a *assetFlagList) String() string { return strings.Join(*a, ",") }

func (a *assetFlagList) Set(v string) error {
	*a = append(*a, v)
	return nil
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

// extractVersion runs the binary with --version and parses the output.
// Expected format: "phaethon v0.7.3-54-g408ea4e" or similar
func extractVersion(binaryPath string) (string, error) {
	cmd := exec.Command(binaryPath, "--version")
	output, err := cmd.Output()
	if err != nil {
		// If we can't run the binary (e.g., cross-compiled), try to extract from strings
		return extractVersionFromStrings(binaryPath)
	}
	// Parse output. Current binaries print
	// "version=<v> platform=<os> arch=<arch>"; older ones printed
	// "phaethon <v>". Handle both, and prefer the explicit version= field:
	// on a host matching the target platform the binary actually runs, so
	// blindly taking Fields()[1] would return "platform=..." instead.
	line := strings.TrimSpace(string(output))
	for _, f := range strings.Fields(line) {
		if v, ok := strings.CutPrefix(f, "version="); ok {
			return v, nil
		}
	}
	parts := strings.Fields(line)
	if len(parts) >= 2 {
		return parts[1], nil
	}
	return "", fmt.Errorf("unexpected version output: %q", line)
}

// extractVersionFromStrings tries to find the version string in the binary
// by searching for common patterns. Used when the binary can't be executed
// (e.g., cross-compiled for a different platform).
func extractVersionFromStrings(binaryPath string) (string, error) {
	// Use strings command to extract printable strings
	cmd := exec.Command("strings", binaryPath)
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("cannot run binary or strings: %w", err)
	}
	// Look for version pattern: v0.7.3-XX-gHASH or similar
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		// Match pattern like "v0.7.3-54-g408ea4e" or "v0.7.3"
		if strings.HasPrefix(line, "v0.") && (strings.Contains(line, "-g") || strings.Count(line, ".") >= 2) {
			// Make sure it looks like a version, not random text
			if len(line) < 30 && !strings.ContainsAny(line, " \t\n\r") {
				return line, nil
			}
		}
	}
	return "", fmt.Errorf("cannot extract version from binary")
}
