package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"phaethon/pkg/signing"
	"phaethon/pkg/upgrade"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestSwapBinaryKeepsStablePath(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "phaethon.exe")
	writeFile(t, exe, "current")
	writeFile(t, exe+".old", "previous")
	writeFile(t, exe+".new", "next")

	backup := prepareBackup(exe)
	if err := swapBinary(exe, exe+".new", backup); err != nil {
		t.Fatalf("swapBinary: %v", err)
	}
	if got := readFile(t, exe); got != "next" {
		t.Fatalf("stable path = %q, want next", got)
	}
	if got := readFile(t, exe+".old"); got != "current" {
		t.Fatalf(".old = %q, want current", got)
	}
	if got := readFile(t, exe+".old2"); got != "previous" {
		t.Fatalf(".old2 = %q, want previous", got)
	}
	if _, err := os.Stat(exe + ".new"); !os.IsNotExist(err) {
		t.Fatalf("staged binary still present: %v", err)
	}
}

func TestRestoreBackupRollsBack(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "phaethon")
	writeFile(t, exe, "bad")
	writeFile(t, exe+".old", "good")

	if err := restoreBackup(exe, ""); err != nil {
		t.Fatalf("restoreBackup: %v", err)
	}
	if got := readFile(t, exe); got != "good" {
		t.Fatalf("restored binary = %q, want good", got)
	}
	if got := readFile(t, exe+".failed"); got != "bad" {
		t.Fatalf(".failed = %q, want bad", got)
	}
}

func TestPrepareBackupFallsBackWhenOldCannotRotate(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "phaethon")
	writeFile(t, exe, "current")
	writeFile(t, exe+".old", "previous")
	// A non-empty .old2 makes both its removal and the .old -> .old2 rename
	// fail, mirroring Windows where .old is the watchdog's mapped image.
	if err := os.MkdirAll(filepath.Join(exe+".old2", "keep"), 0755); err != nil {
		t.Fatal(err)
	}

	backup := prepareBackup(exe)
	if backup != exe+".old.last" {
		t.Fatalf("backup = %q, want %q", backup, exe+".old.last")
	}

	writeFile(t, exe+".new", "next")
	if err := swapBinary(exe, exe+".new", backup); err != nil {
		t.Fatalf("swapBinary: %v", err)
	}
	if got := readFile(t, exe); got != "next" {
		t.Fatalf("stable path = %q, want next", got)
	}
	if got := readFile(t, exe+".old"); got != "previous" {
		t.Fatalf(".old = %q, want previous (untouched)", got)
	}
	if got := readFile(t, backup); got != "current" {
		t.Fatalf("%s = %q, want current", backup, got)
	}

	if err := restoreBackup(exe, backup); err != nil {
		t.Fatalf("restoreBackup: %v", err)
	}
	if got := readFile(t, exe); got != "current" {
		t.Fatalf("restored binary = %q, want current", got)
	}
	if got := readFile(t, exe+".failed"); got != "next" {
		t.Fatalf(".failed = %q, want next", got)
	}
}

func TestRepairRestoresMissingExecutableFromBackup(t *testing.T) {
	t.Chdir(t.TempDir())
	exe := filepath.Join(mustGetwd(t), "phaethon")
	good := strings.Repeat("g", minRestoredBinarySize)
	staged := strings.Repeat("n", minRestoredBinarySize)
	writeFile(t, exe+".old", good)
	writeFile(t, exe+".new", staged)
	if err := upgrade.WriteMarker(upgrade.Marker{Old: exe + ".old", New: exe + ".new", Version: "v665"}); err != nil {
		t.Fatal(err)
	}
	if err := upgrade.WriteRequest(upgrade.Request{Version: "v665"}); err != nil {
		t.Fatal(err)
	}

	repairInterruptedUpgrade(exe)

	if got := readFile(t, exe); got != good {
		t.Fatalf("restored executable length = %d, want %d", len(got), len(good))
	}
	if _, err := os.Stat(exe + ".new"); !os.IsNotExist(err) {
		t.Fatalf("staged binary still present: %v", err)
	}
	if upgrade.ReadMarker() != nil {
		t.Fatal("marker not cleared")
	}
	if upgrade.ReadRequest() != nil {
		t.Fatal("request should be cleared after restoring a broken swap")
	}
	if st := upgrade.ReadStatus(); st.Status != upgrade.StatusFailedRolledBack {
		t.Fatalf("status = %q, want %q", st.Status, upgrade.StatusFailedRolledBack)
	}
}

func TestRepairDropsStaleStagingWhenExecutableExists(t *testing.T) {
	t.Chdir(t.TempDir())
	exe := filepath.Join(mustGetwd(t), "phaethon")
	writeFile(t, exe, "current")
	writeFile(t, exe+".new", "stale")

	repairInterruptedUpgrade(exe)

	if _, err := os.Stat(exe + ".new"); !os.IsNotExist(err) {
		t.Fatalf("stale .new not removed: %v", err)
	}
	if got := readFile(t, exe); got != "current" {
		t.Fatalf("executable changed: %q", got)
	}
}

func TestFindPkgForVersionMatchesSignedPackage(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("data/packages", 0755); err != nil {
		t.Fatal(err)
	}
	oldPlatform, oldArch := Platform, Arch
	Platform, Arch = "linux", "amd64"
	defer func() { Platform, Arch = oldPlatform, oldArch }()

	_, priv := signing.GenerateTestKeyPair()
	binary := []byte("fake-binary")
	meta := signing.PackageMeta{Version: "v0.8.0-665", Platform: "linux", Arch: "amd64"}
	metaJSON, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	sig := signing.SignData(binary, metaJSON, priv)
	if err := signing.CreatePkg(filepath.Join("data/packages", "abc.pkg"), binary, meta, sig); err != nil {
		t.Fatal(err)
	}

	pkgPath, contents, err := findPkgForVersion("v0.8.0-665")
	if err != nil {
		t.Fatalf("findPkgForVersion: %v", err)
	}
	if pkgPath == "" || contents.Meta.Version != "v0.8.0-665" {
		t.Fatalf("unexpected package: %s %+v", pkgPath, contents.Meta)
	}
	if _, _, err := findPkgForVersion("v0.8.0-666"); err == nil {
		t.Fatal("expected error for missing version")
	}
}

func TestTryApplyAlreadyRunningMarksSucceeded(t *testing.T) {
	t.Chdir(t.TempDir())
	exe := filepath.Join(mustGetwd(t), "phaethon")
	if err := upgrade.WriteRequest(upgrade.Request{Version: "v665"}); err != nil {
		t.Fatal(err)
	}
	if err := upgrade.WriteStatus(upgrade.Status{Status: upgrade.StatusApplying, Requested: "v665"}); err != nil {
		t.Fatal(err)
	}
	cp := &childProcess{done: make(chan struct{})}
	cp.ready.Store(true)
	cp.version.Store("v665")

	newCp, _, handled, _ := tryApplyUpgrade(exe, cp, 0, nil)
	if !handled || newCp != nil {
		t.Fatalf("handled=%v newCp=%v, want true/nil", handled, newCp)
	}
	if upgrade.ReadRequest() != nil {
		t.Fatal("request not cleared")
	}
	if st := upgrade.ReadStatus(); st.Status != upgrade.StatusSucceeded || st.Current != "v665" {
		t.Fatalf("status = %+v", st)
	}
}

func TestTryApplyMissingPackageDoesNotStopWorker(t *testing.T) {
	t.Chdir(t.TempDir())
	exe := filepath.Join(mustGetwd(t), "phaethon")
	if err := upgrade.WriteRequest(upgrade.Request{Version: "v9"}); err != nil {
		t.Fatal(err)
	}
	cp := &childProcess{done: make(chan struct{})}
	cp.ready.Store(true)

	newCp, _, handled, _ := tryApplyUpgrade(exe, cp, 0, nil)
	if !handled || newCp != nil {
		t.Fatalf("handled=%v newCp=%v, want true/nil", handled, newCp)
	}
	if st := upgrade.ReadStatus(); st.Status != upgrade.StatusFailedRolledBack || st.LastError == "" {
		t.Fatalf("status = %+v", st)
	}
	if upgrade.ReadRequest() != nil {
		t.Fatal("request not cleared")
	}
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}
