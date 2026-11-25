package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"phaethon/pkg/signing"
	"phaethon/pkg/upgrade"
	"phaethon/util"
)

// Asset files are staged next to the executable with this suffix and only
// renamed into place after the worker exits, so a mapped winpty.dll never has
// to be overwritten in place.
const (
	assetNewSuffix = ".asset-new"
	assetOldSuffix = ".asset-old"
)

// maybeStageAssets decides whether this package's assets need to be installed
// and, if so, verifies and writes them next to the executable. Staging failure
// aborts the upgrade before the worker is touched.
func maybeStageAssets(exe string, contents *signing.PkgContents) ([]upgrade.AssetMarker, error) {
	if len(contents.Assets) == 0 || !shouldInstallAssets(filepath.Dir(exe), contents.Assets) {
		return nil, nil
	}
	return stageAssets(filepath.Dir(exe), contents)
}

// stageAssets writes every asset to <dir>/<name>.asset-new, fsyncing before
// returning. The digests are checked again here: the package signature already
// covers meta.json, but a size/hash mismatch must never reach the deployment.
func stageAssets(dir string, contents *signing.PkgContents) (markers []upgrade.AssetMarker, err error) {
	declared := make(map[string]signing.AssetMeta, len(contents.Meta.Assets))
	for _, a := range contents.Meta.Assets {
		declared[a.Name] = a
	}
	names := make([]string, 0, len(contents.Assets))
	for name := range contents.Assets {
		names = append(names, name)
	}
	sort.Strings(names)

	cleanup := func() {
		for _, m := range markers {
			_ = os.Remove(m.New)
		}
	}
	defer func() {
		if err != nil {
			cleanup()
		}
	}()

	for _, name := range names {
		data := contents.Assets[name]
		want, ok := declared[name]
		if !ok {
			return nil, fmt.Errorf("asset %s not listed in meta.json", name)
		}
		sum := sha256.Sum256(data)
		if int64(len(data)) != want.Size || !strings.EqualFold(hex.EncodeToString(sum[:]), want.SHA256) {
			return nil, fmt.Errorf("asset %s digest mismatch", name)
		}

		newPath := filepath.Join(dir, name+assetNewSuffix)
		_ = os.Remove(newPath)
		f, err := os.OpenFile(newPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			return nil, fmt.Errorf("create staged asset %s: %w", name, err)
		}
		if _, err := f.Write(data); err != nil {
			f.Close()
			return nil, fmt.Errorf("write staged asset %s: %w", name, err)
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return nil, fmt.Errorf("sync staged asset %s: %w", name, err)
		}
		if err := f.Close(); err != nil {
			return nil, fmt.Errorf("close staged asset %s: %w", name, err)
		}
		syncParentDir(newPath)

		markers = append(markers, upgrade.AssetMarker{
			Name:   name,
			New:    newPath,
			Backup: filepath.Join(dir, name+assetOldSuffix),
		})
	}
	return markers, nil
}

// installAssets swaps staged assets into place. Failures are warnings only:
// the executable upgrade must still proceed, the node just keeps a degraded
// PTY until the next successful install.
func installAssets(markers []upgrade.AssetMarker) {
	for _, m := range markers {
		target := filepath.Join(filepath.Dir(m.New), m.Name)
		if fileExists(target) {
			_ = os.Remove(m.Backup)
			if err := os.Rename(target, m.Backup); err != nil {
				upgradeLog("asset %s: cannot move current aside: %v", m.Name, err)
				util.LogModuleWarn("main", "watchdog: asset %s install skipped: %v", m.Name, err)
				continue
			}
		}
		if err := os.Rename(m.New, target); err != nil {
			// Put the previous file back when possible rather than leaving a hole.
			_ = os.Rename(m.Backup, target)
			upgradeLog("asset %s: install failed: %v", m.Name, err)
			util.LogModuleWarn("main", "watchdog: asset %s install failed: %v", m.Name, err)
			continue
		}
		syncParentDir(target)
		upgradeLog("asset installed: %s", target)
	}
}

// removeStagedAssets drops staged files that were never installed (upgrade
// aborted or interrupted). Installed assets are left untouched: they are
// version-independent and may already be in use.
func removeStagedAssets(markers []upgrade.AssetMarker) {
	for _, m := range markers {
		_ = os.Remove(m.New)
	}
}

// cleanupAssetResidue removes *.asset-new files left by an interrupted swap.
// Assets are never rolled back, so recovery only needs to delete the debris.
func cleanupAssetResidue(dir string) {
	matches, err := filepath.Glob(filepath.Join(dir, "*"+assetNewSuffix))
	if err != nil {
		return
	}
	for _, path := range matches {
		if err := os.Remove(path); err == nil {
			upgradeLog("repair: removed staged asset residue %s", path)
		}
	}
}
