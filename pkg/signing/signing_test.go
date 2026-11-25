package signing

import (
	"archive/zip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func assetMetaFor(name string, data []byte) AssetMeta {
	sum := sha256.Sum256(data)
	return AssetMeta{Name: name, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
}

func signMeta(t *testing.T, binary []byte, meta PackageMeta, priv ed25519.PrivateKey) []byte {
	t.Helper()
	metaJSON, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return SignData(binary, metaJSON, priv)
}

// writeRawPkg builds a package directly so tests can inject entries that the
// normal CreatePkg path refuses to write.
func writeRawPkg(t *testing.T, path string, binary []byte, meta PackageMeta, sig []byte, entries map[string][]byte) {
	t.Helper()
	metaJSON, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	writeEntry := func(name string, data []byte) {
		ew, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ew.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	writeEntry("binary", binary)
	writeEntry("meta.json", metaJSON)
	writeEntry("signature", []byte(base64.StdEncoding.EncodeToString(sig)))
	for name, data := range entries {
		writeEntry(name, data)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCreatePkgWithAssetsRoundTrip(t *testing.T) {
	pub, priv := GenerateTestKeyPair()
	binary := []byte("binary-bytes")
	dll := []byte("dll-bytes")
	agent := []byte("agent-bytes")
	meta := PackageMeta{
		Version:  "v-test",
		Platform: "windows",
		Arch:     "amd64",
		Assets:   []AssetMeta{assetMetaFor("winpty.dll", dll), assetMetaFor("winpty-agent.exe", agent)},
	}
	sig := signMeta(t, binary, meta, priv)
	pkgPath := filepath.Join(t.TempDir(), "a.pkg")
	assets := map[string][]byte{"winpty.dll": dll, "winpty-agent.exe": agent}
	if err := CreatePkgWithAssets(pkgPath, binary, meta, sig, assets); err != nil {
		t.Fatalf("CreatePkgWithAssets: %v", err)
	}

	contents, err := VerifyPkg(pkgPath, pub)
	if err != nil {
		t.Fatalf("VerifyPkg: %v", err)
	}
	if string(contents.Assets["winpty.dll"]) != string(dll) || string(contents.Assets["winpty-agent.exe"]) != string(agent) {
		t.Fatalf("asset contents not preserved: %+v", contents.Assets)
	}
	if len(contents.Meta.Assets) != 2 {
		t.Fatalf("meta assets = %d, want 2", len(contents.Meta.Assets))
	}

	metaOnly, err := VerifyPkgMeta(pkgPath, pub)
	if err != nil {
		t.Fatalf("VerifyPkgMeta: %v", err)
	}
	if len(metaOnly.Assets) != 2 {
		t.Fatalf("VerifyPkgMeta assets = %d, want 2", len(metaOnly.Assets))
	}

	raw, err := os.ReadFile(pkgPath)
	if err != nil {
		t.Fatal(err)
	}
	fromMem, err := VerifyPkgBytes(raw, pub)
	if err != nil {
		t.Fatalf("VerifyPkgBytes: %v", err)
	}
	if string(fromMem.Assets["winpty-agent.exe"]) != string(agent) {
		t.Fatal("VerifyPkgBytes lost asset contents")
	}
}

func TestVerifyRejectsUndeclaredAsset(t *testing.T) {
	pub, priv := GenerateTestKeyPair()
	binary := []byte("binary-bytes")
	meta := PackageMeta{Version: "v-test", Platform: "windows", Arch: "amd64"}
	sig := signMeta(t, binary, meta, priv)
	pkgPath := filepath.Join(t.TempDir(), "a.pkg")
	writeRawPkg(t, pkgPath, binary, meta, sig, map[string][]byte{"assets/winpty.dll": []byte("x")})

	if _, err := VerifyPkg(pkgPath, pub); err == nil {
		t.Fatal("expected undeclared asset to be rejected")
	}
	if _, err := VerifyPkgMeta(pkgPath, pub); err == nil {
		t.Fatal("expected VerifyPkgMeta to reject undeclared asset")
	}
}

func TestVerifyRejectsMissingAsset(t *testing.T) {
	pub, priv := GenerateTestKeyPair()
	binary := []byte("binary-bytes")
	dll := []byte("dll-bytes")
	meta := PackageMeta{
		Version:  "v-test",
		Platform: "windows",
		Arch:     "amd64",
		Assets:   []AssetMeta{assetMetaFor("winpty.dll", dll)},
	}
	sig := signMeta(t, binary, meta, priv)
	pkgPath := filepath.Join(t.TempDir(), "a.pkg")
	writeRawPkg(t, pkgPath, binary, meta, sig, nil)

	if _, err := VerifyPkg(pkgPath, pub); err == nil {
		t.Fatal("expected missing asset to be rejected")
	}
}

func TestVerifyRejectsTamperedAsset(t *testing.T) {
	pub, priv := GenerateTestKeyPair()
	binary := []byte("binary-bytes")
	dll := []byte("dll-bytes")
	meta := PackageMeta{
		Version:  "v-test",
		Platform: "windows",
		Arch:     "amd64",
		Assets:   []AssetMeta{assetMetaFor("winpty.dll", dll)},
	}
	sig := signMeta(t, binary, meta, priv)
	pkgPath := filepath.Join(t.TempDir(), "a.pkg")
	writeRawPkg(t, pkgPath, binary, meta, sig, map[string][]byte{"assets/winpty.dll": []byte("evil")})

	if _, err := VerifyPkg(pkgPath, pub); err == nil {
		t.Fatal("expected tampered asset to be rejected")
	}
}

func TestVerifyRejectsNonWhitelistedAssetEntry(t *testing.T) {
	pub, priv := GenerateTestKeyPair()
	binary := []byte("binary-bytes")
	meta := PackageMeta{Version: "v-test", Platform: "windows", Arch: "amd64"}
	sig := signMeta(t, binary, meta, priv)
	pkgPath := filepath.Join(t.TempDir(), "a.pkg")
	writeRawPkg(t, pkgPath, binary, meta, sig, map[string][]byte{"assets/evil.exe": []byte("x")})

	if _, err := VerifyPkg(pkgPath, pub); err == nil {
		t.Fatal("expected non-whitelisted asset to be rejected")
	}
}

func TestVerifyRejectsAssetTraversal(t *testing.T) {
	pub, priv := GenerateTestKeyPair()
	binary := []byte("binary-bytes")
	meta := PackageMeta{Version: "v-test", Platform: "windows", Arch: "amd64"}
	sig := signMeta(t, binary, meta, priv)
	pkgPath := filepath.Join(t.TempDir(), "a.pkg")
	writeRawPkg(t, pkgPath, binary, meta, sig, map[string][]byte{"assets/../winpty.dll": []byte("x")})

	if _, err := VerifyPkg(pkgPath, pub); err == nil {
		t.Fatal("expected traversal asset path to be rejected")
	}
}

func TestCreatePkgRejectsOversizedAsset(t *testing.T) {
	_, priv := GenerateTestKeyPair()
	binary := []byte("binary-bytes")
	meta := PackageMeta{
		Version:  "v-test",
		Platform: "windows",
		Arch:     "amd64",
		Assets:   []AssetMeta{{Name: "winpty.dll", Size: maxAssetSize + 1, SHA256: "deadbeef"}},
	}
	sig := signMeta(t, binary, meta, priv)
	err := CreatePkgWithAssets(filepath.Join(t.TempDir(), "a.pkg"), binary, meta, sig, map[string][]byte{"winpty.dll": []byte("x")})
	if err == nil {
		t.Fatal("expected oversized asset to be rejected")
	}
}

func TestCreatePkgRejectsNonWhitelistedAsset(t *testing.T) {
	_, priv := GenerateTestKeyPair()
	binary := []byte("binary-bytes")
	data := []byte("x")
	meta := PackageMeta{
		Version:  "v-test",
		Platform: "windows",
		Arch:     "amd64",
		Assets:   []AssetMeta{assetMetaFor("evil.exe", data)},
	}
	sig := signMeta(t, binary, meta, priv)
	err := CreatePkgWithAssets(filepath.Join(t.TempDir(), "a.pkg"), binary, meta, sig, map[string][]byte{"evil.exe": data})
	if err == nil {
		t.Fatal("expected non-whitelisted asset name to be rejected")
	}
}

func TestCreatePkgWithoutAssetsStillVerifies(t *testing.T) {
	pub, priv := GenerateTestKeyPair()
	binary := []byte("binary-bytes")
	meta := PackageMeta{Version: "v-test", Platform: "linux", Arch: "amd64"}
	sig := signMeta(t, binary, meta, priv)
	pkgPath := filepath.Join(t.TempDir(), "a.pkg")
	if err := CreatePkg(pkgPath, binary, meta, sig); err != nil {
		t.Fatalf("CreatePkg: %v", err)
	}
	contents, err := VerifyPkg(pkgPath, pub)
	if err != nil {
		t.Fatalf("VerifyPkg: %v", err)
	}
	if len(contents.Assets) != 0 {
		t.Fatalf("assets = %d, want 0", len(contents.Assets))
	}
}
