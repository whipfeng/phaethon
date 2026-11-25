package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"phaethon/pkg/signing"
)

func assetContents(name string, data []byte) *signing.PkgContents {
	sum := sha256.Sum256(data)
	return &signing.PkgContents{
		Binary: []byte("binary"),
		Meta: signing.PackageMeta{
			Version: "v-test",
			Assets:  []signing.AssetMeta{{Name: name, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}},
		},
		Assets: map[string][]byte{name: data},
	}
}

func TestStageAndInstallAssets(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "winpty.dll")
	writeFile(t, target, "old")

	markers, err := stageAssets(dir, assetContents("winpty.dll", []byte("new")))
	if err != nil {
		t.Fatalf("stageAssets: %v", err)
	}
	if len(markers) != 1 {
		t.Fatalf("markers = %d, want 1", len(markers))
	}
	if got := readFile(t, target+assetNewSuffix); got != "new" {
		t.Fatalf("staged content = %q, want new", got)
	}

	installAssets(markers)

	if got := readFile(t, target); got != "new" {
		t.Fatalf("installed content = %q, want new", got)
	}
	if got := readFile(t, target+assetOldSuffix); got != "old" {
		t.Fatalf("backup content = %q, want old", got)
	}
	if _, err := os.Stat(target + assetNewSuffix); !os.IsNotExist(err) {
		t.Fatalf("staged asset still present: %v", err)
	}
}

func TestInstallAssetsWithoutExistingTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "winpty-agent.exe")

	markers, err := stageAssets(dir, assetContents("winpty-agent.exe", []byte("agent")))
	if err != nil {
		t.Fatalf("stageAssets: %v", err)
	}
	installAssets(markers)

	if got := readFile(t, target); got != "agent" {
		t.Fatalf("installed content = %q, want agent", got)
	}
	if fileExists(target + assetOldSuffix) {
		t.Fatal("unexpected backup for a fresh asset")
	}
}

func TestStageAssetsRejectsDigestMismatch(t *testing.T) {
	dir := t.TempDir()
	contents := assetContents("winpty.dll", []byte("good"))
	contents.Assets["winpty.dll"] = []byte("tampered")

	if _, err := stageAssets(dir, contents); err == nil {
		t.Fatal("expected digest mismatch to be rejected")
	}
	if _, err := os.Stat(filepath.Join(dir, "winpty.dll"+assetNewSuffix)); !os.IsNotExist(err) {
		t.Fatalf("staged residue left behind: %v", err)
	}
}

func TestCleanupAssetResidue(t *testing.T) {
	dir := t.TempDir()
	residue := filepath.Join(dir, "winpty.dll"+assetNewSuffix)
	writeFile(t, residue, "stale")

	cleanupAssetResidue(dir)

	if _, err := os.Stat(residue); !os.IsNotExist(err) {
		t.Fatalf("residue not removed: %v", err)
	}
}

func TestRepairRemovesAssetResidue(t *testing.T) {
	t.Chdir(t.TempDir())
	exe := filepath.Join(mustGetwd(t), "phaethon")
	writeFile(t, exe, "current")
	assetResidue := filepath.Join(filepath.Dir(exe), "winpty.dll"+assetNewSuffix)
	writeFile(t, assetResidue, "stale")

	repairInterruptedUpgrade(exe)

	if _, err := os.Stat(assetResidue); !os.IsNotExist(err) {
		t.Fatalf("asset residue not removed: %v", err)
	}
	if got := readFile(t, exe); got != "current" {
		t.Fatalf("executable changed: %q", got)
	}
}
