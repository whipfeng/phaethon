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
	"sort"
	"strings"
)

// PublicKey is the hardcoded Ed25519 public key for package signature verification.
// Only packages signed with the corresponding private key are accepted.
// Seed:    0x000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f
// Public:  0x03a107bff3ce10be1d70dd18e74bc09967e4d6309ba50d5f1ddc8664125531b8
var PublicKey = func() ed25519.PublicKey {
	pubHex := "03a107bff3ce10be1d70dd18e74bc09967e4d6309ba50d5f1ddc8664125531b8"
	pubBytes, _ := hex.DecodeString(pubHex)
	return ed25519.PublicKey(pubBytes)
}()

// PackageMeta represents the metadata in a .pkg file.
type PackageMeta struct {
	Version   string      `json:"version"`
	Platform  string      `json:"platform"`
	Arch      string      `json:"arch"`
	BuildTime string      `json:"buildTime,omitempty"`
	GitCommit string      `json:"gitCommit,omitempty"`
	GoVersion string      `json:"goVersion,omitempty"`
	BuildTag  string      `json:"buildTag,omitempty"`
	Assets    []AssetMeta `json:"assets,omitempty"`
}

// AssetMeta describes an auxiliary file shipped under assets/. The digest is
// part of meta.json and therefore covered by the existing package signature.
type AssetMeta struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// PkgContents represents the extracted contents of a .pkg file.
type PkgContents struct {
	Binary    []byte
	Meta      PackageMeta
	Signature []byte
	// Assets maps a whitelisted filename to its contents.
	Assets map[string][]byte
}

const (
	maxAssetSize   = 16 << 20 // per file, guards against zip bombs
	maxAssetsTotal = 32 << 20
)

// allowedAssetNames is the closed set of files the watchdog may install next
// to the executable. Names are plain filenames: no directories, no traversal.
var allowedAssetNames = map[string]struct{}{
	"winpty.dll":       {},
	"winpty-agent.exe": {},
}

// classifyPkgEntry validates an entry path and reports its kind. assetName is
// only set for "asset" entries.
func classifyPkgEntry(f *zip.File) (kind, assetName string, err error) {
	cleanName := filepath.Clean(f.Name)
	if filepath.IsAbs(cleanName) {
		return "", "", fmt.Errorf("invalid path in pkg: %s (absolute)", f.Name)
	}
	if strings.HasPrefix(cleanName, "..") || strings.Contains(cleanName, string(filepath.Separator)+"..") {
		return "", "", fmt.Errorf("invalid path in pkg: %s (path traversal)", f.Name)
	}
	switch cleanName {
	case "binary", "meta.json", "signature":
		return cleanName, "", nil
	}
	slash := filepath.ToSlash(cleanName)
	if name, ok := strings.CutPrefix(slash, "assets/"); ok {
		if _, allowed := allowedAssetNames[name]; allowed {
			return "asset", name, nil
		}
	}
	return "", "", fmt.Errorf("unexpected file in pkg: %s", f.Name)
}

// verifyAssetManifest checks that the zip assets exactly match the signed
// meta.json list: every declared asset is present with the recorded size and
// digest, and no undeclared asset is shipped.
func verifyAssetManifest(declared []AssetMeta, present map[string]AssetMeta) error {
	if len(declared) == 0 {
		if len(present) > 0 {
			return fmt.Errorf("pkg contains assets not listed in meta.json")
		}
		return nil
	}
	seen := make(map[string]struct{}, len(declared))
	var total int64
	for _, a := range declared {
		if _, ok := allowedAssetNames[a.Name]; !ok {
			return fmt.Errorf("unexpected asset in meta.json: %s", a.Name)
		}
		if _, dup := seen[a.Name]; dup {
			return fmt.Errorf("duplicate asset in meta.json: %s", a.Name)
		}
		seen[a.Name] = struct{}{}
		if a.Size < 0 || a.Size > maxAssetSize {
			return fmt.Errorf("asset %s size %d out of range", a.Name, a.Size)
		}
		total += a.Size
		if total > maxAssetsTotal {
			return fmt.Errorf("assets exceed %d bytes", maxAssetsTotal)
		}
		got, ok := present[a.Name]
		if !ok {
			return fmt.Errorf("missing asset in pkg: %s", a.Name)
		}
		if got.Size != a.Size {
			return fmt.Errorf("asset %s size mismatch: meta=%d pkg=%d", a.Name, a.Size, got.Size)
		}
		if !strings.EqualFold(got.SHA256, a.SHA256) {
			return fmt.Errorf("asset %s hash mismatch", a.Name)
		}
	}
	for name := range present {
		if _, ok := seen[name]; !ok {
			return fmt.Errorf("pkg contains asset not listed in meta.json: %s", name)
		}
	}
	return nil
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
	assets := map[string][]byte{}
	assetMeta := map[string]AssetMeta{}

	// Extract files
	for _, f := range r.File {
		kind, assetName, err := classifyPkgEntry(f)
		if err != nil {
			return nil, err
		}

		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", f.Name, err)
		}

		var data []byte
		if kind == "asset" {
			data, err = io.ReadAll(io.LimitReader(rc, maxAssetSize+1))
		} else {
			data, err = io.ReadAll(rc)
		}
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", f.Name, err)
		}

		switch kind {
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
		case "asset":
			if _, dup := assets[assetName]; dup {
				return nil, fmt.Errorf("duplicate asset in pkg: %s", assetName)
			}
			if int64(len(data)) > maxAssetSize {
				return nil, fmt.Errorf("asset %s exceeds %d bytes", assetName, maxAssetSize)
			}
			sum := sha256.Sum256(data)
			assets[assetName] = data
			assetMeta[assetName] = AssetMeta{Name: assetName, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
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
	if err := verifyAssetManifest(meta.Assets, assetMeta); err != nil {
		return nil, err
	}

	return &PkgContents{
		Binary:    binary,
		Meta:      meta,
		Signature: signature,
		Assets:    assets,
	}, nil
}

// VerifyPkgMeta verifies a .pkg signature and returns only its metadata,
// streaming the binary through the hash instead of retaining it in memory.
// Used by upload/receive paths where the package stays on disk and is verified
// again by the watchdog before installation.
func VerifyPkgMeta(pkgPath string, publicKey ed25519.PublicKey) (*PackageMeta, error) {
	r, err := zip.OpenReader(pkgPath)
	if err != nil {
		return nil, fmt.Errorf("open pkg: %w", err)
	}
	defer r.Close()

	const maxBinary = 128 << 20 // 128 MiB, same bound as the binary cache
	var meta PackageMeta
	var metaRaw, signature []byte
	haveBinary := false
	hasher := sha256.New()
	assetMeta := map[string]AssetMeta{}

	for _, f := range r.File {
		kind, assetName, err := classifyPkgEntry(f)
		if err != nil {
			return nil, err
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", f.Name, err)
		}
		switch kind {
		case "binary":
			n, err := io.Copy(hasher, io.LimitReader(rc, maxBinary+1))
			rc.Close()
			if err != nil {
				return nil, fmt.Errorf("read binary: %w", err)
			}
			if n > maxBinary {
				return nil, fmt.Errorf("binary exceeds %d bytes", maxBinary)
			}
			haveBinary = true
		case "meta.json":
			data, err := io.ReadAll(io.LimitReader(rc, 1<<20))
			rc.Close()
			if err != nil {
				return nil, fmt.Errorf("read meta.json: %w", err)
			}
			metaRaw = data
			if err := json.Unmarshal(data, &meta); err != nil {
				return nil, fmt.Errorf("parse meta.json: %w", err)
			}
		case "signature":
			data, err := io.ReadAll(io.LimitReader(rc, 1<<20))
			rc.Close()
			if err != nil {
				return nil, fmt.Errorf("read signature: %w", err)
			}
			signature, err = base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
			if err != nil {
				return nil, fmt.Errorf("decode signature: %w", err)
			}
		case "asset":
			if _, dup := assetMeta[assetName]; dup {
				rc.Close()
				return nil, fmt.Errorf("duplicate asset in pkg: %s", assetName)
			}
			h := sha256.New()
			n, err := io.Copy(h, io.LimitReader(rc, maxAssetSize+1))
			rc.Close()
			if err != nil {
				return nil, fmt.Errorf("read asset %s: %w", assetName, err)
			}
			if n > maxAssetSize {
				return nil, fmt.Errorf("asset %s exceeds %d bytes", assetName, maxAssetSize)
			}
			assetMeta[assetName] = AssetMeta{Name: assetName, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}
		}
	}
	if !haveBinary {
		return nil, fmt.Errorf("missing binary in pkg")
	}
	if metaRaw == nil {
		return nil, fmt.Errorf("missing meta.json in pkg")
	}
	if signature == nil {
		return nil, fmt.Errorf("missing signature in pkg")
	}

	// Signature covers sha256(binary + meta.json); the binary was streamed into
	// the hasher first, so append the exact meta bytes now.
	hasher.Write(metaRaw)
	if !ed25519.Verify(publicKey, hasher.Sum(nil), signature) {
		return nil, fmt.Errorf("signature verification failed")
	}
	if err := verifyAssetManifest(meta.Assets, assetMeta); err != nil {
		return nil, err
	}
	return &meta, nil
}

// CreatePkg creates a .pkg file from binary, metadata, and signature.
// Used for testing and build tools.
func CreatePkg(pkgPath string, binary []byte, meta PackageMeta, signature []byte) error {
	return CreatePkgWithAssets(pkgPath, binary, meta, signature, nil)
}

// CreatePkgWithAssets creates a .pkg that additionally carries the given
// assets under assets/. Every entry must be declared in meta.Assets with a
// matching size and digest; the caller marshals meta.json for signing before
// calling this, so the manifest is covered by the signature.
func CreatePkgWithAssets(pkgPath string, binary []byte, meta PackageMeta, signature []byte, assets map[string][]byte) error {
	declared := make(map[string]AssetMeta, len(meta.Assets))
	var total int64
	for _, a := range meta.Assets {
		if _, ok := allowedAssetNames[a.Name]; !ok {
			return fmt.Errorf("unexpected asset in meta.json: %s", a.Name)
		}
		if _, dup := declared[a.Name]; dup {
			return fmt.Errorf("duplicate asset in meta.json: %s", a.Name)
		}
		if a.Size < 0 || a.Size > maxAssetSize {
			return fmt.Errorf("asset %s size %d out of range", a.Name, a.Size)
		}
		total += a.Size
		if total > maxAssetsTotal {
			return fmt.Errorf("assets exceed %d bytes", maxAssetsTotal)
		}
		declared[a.Name] = a
	}
	if len(declared) != len(assets) {
		return fmt.Errorf("assets do not match meta.json manifest")
	}
	names := make([]string, 0, len(assets))
	for name, data := range assets {
		a, ok := declared[name]
		if !ok {
			return fmt.Errorf("asset %s not listed in meta.json", name)
		}
		sum := sha256.Sum256(data)
		if a.Size != int64(len(data)) || !strings.EqualFold(a.SHA256, hex.EncodeToString(sum[:])) {
			return fmt.Errorf("asset %s does not match meta.json", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)

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

	for _, name := range names {
		aw, err := w.Create("assets/" + name)
		if err != nil {
			return err
		}
		if _, err := aw.Write(assets[name]); err != nil {
			return err
		}
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
	assets := map[string][]byte{}
	assetMeta := map[string]AssetMeta{}

	for _, f := range r.File {
		kind, assetName, err := classifyPkgEntry(f)
		if err != nil {
			return nil, err
		}

		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", f.Name, err)
		}

		var data []byte
		if kind == "asset" {
			data, err = io.ReadAll(io.LimitReader(rc, maxAssetSize+1))
		} else {
			data, err = io.ReadAll(rc)
		}
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", f.Name, err)
		}

		switch kind {
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
		case "asset":
			if _, dup := assets[assetName]; dup {
				return nil, fmt.Errorf("duplicate asset in pkg: %s", assetName)
			}
			if int64(len(data)) > maxAssetSize {
				return nil, fmt.Errorf("asset %s exceeds %d bytes", assetName, maxAssetSize)
			}
			sum := sha256.Sum256(data)
			assets[assetName] = data
			assetMeta[assetName] = AssetMeta{Name: assetName, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
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
	if err := verifyAssetManifest(meta.Assets, assetMeta); err != nil {
		return nil, err
	}

	return &PkgContents{
		Binary:    binary,
		Meta:      meta,
		Signature: signature,
		Assets:    assets,
	}, nil
}
