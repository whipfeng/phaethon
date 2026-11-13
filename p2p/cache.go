package p2p

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"phaethon/util"
)

// CacheEntry represents a single binary in the P2P cache.
type CacheEntry struct {
	Platform string `json:"platform"`
	Arch     string `json:"arch"`
	Version  string `json:"version"`
	BuildTag string `json:"buildTag,omitempty"`
}

// BinaryCache manages the p2p-cache/ directory.
type BinaryCache struct {
	dir string
}

// NewBinaryCache creates or opens the p2p-cache directory under workdir.
func NewBinaryCache(workdir string) (*BinaryCache, error) {
	dir := filepath.Join(workdir, "cache", "p2p-cache")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}
	return &BinaryCache{dir: dir}, nil
}

// Dir returns the cache directory path.
func (c *BinaryCache) Dir() string {
	return c.dir
}

// validateCacheKey rejects components containing path separators or ".."
// to prevent path traversal from untrusted peer input.
func validateCacheKey(platform, arch, buildTag, version string) error {
	for _, s := range []string{platform, arch, buildTag, version} {
		if strings.ContainsAny(s, "/\\") || strings.Contains(s, "..") {
			return fmt.Errorf("invalid cache key component: %q", s)
		}
	}
	return nil
}

// cacheFileName builds the filename for a cache entry.
// Format: phaethon_{platform}_{arch}[_{buildTag}]_{version}
func cacheFileName(platform, arch, buildTag, version string) string {
	if buildTag != "" {
		return fmt.Sprintf("phaethon_%s_%s_%s_%s", platform, arch, buildTag, version)
	}
	return fmt.Sprintf("phaethon_%s_%s_%s", platform, arch, version)
}

// FilePath returns the full path for a cache entry.
func (c *BinaryCache) FilePath(platform, arch, buildTag, version string) (string, error) {
	if err := validateCacheKey(platform, arch, buildTag, version); err != nil {
		return "", err
	}
	return filepath.Join(c.dir, cacheFileName(platform, arch, buildTag, version)), nil
}

// SeedOwnBinary copies the running binary into the cache if not already present.
// It also computes sha256 and writes a .sha256 sidecar file for fast admin queries.
func (c *BinaryCache) SeedOwnBinary(version, platform, arch, buildTag string) error {
	target, err := c.FilePath(platform, arch, buildTag, version)
	if err != nil {
		return err
	}
	if _, err := os.Stat(target); err == nil {
		util.LogModuleDebug("p2p", "[P2P] cache: own binary already cached: %s", filepath.Base(target))
		return nil
	}

	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("get executable path: %w", err)
	}

	src, err := os.Open(execPath)
	if err != nil {
		return fmt.Errorf("open executable: %w", err)
	}
	defer src.Close()

	tmp, err := os.CreateTemp(c.dir, filepath.Base(target)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()

	// Compute sha256 while copying
	hasher := sha256.New()
	if _, err := io.Copy(tmp, io.TeeReader(src, hasher)); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("copy binary: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("sync cache file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close cache file: %w", err)
	}

	if err := os.Rename(tmpPath, target); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename to cache: %w", err)
	}

	// Write sha256 sidecar
	hashHex := hex.EncodeToString(hasher.Sum(nil))
	writeSHA256Sidecar(target, hashHex)

	util.LogModuleInfo("p2p", "[P2P] cache: seeded own binary: %s (sha256: %s)", filepath.Base(target), hashHex[:16]+"...")
	return nil
}

// CacheEntryInfo is a cache entry plus its file metadata (for admin display).
type CacheEntryInfo struct {
	CacheEntry
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime"`
}

// Inventory scans the cache directory and returns all entries with file metadata.
func (c *BinaryCache) Inventory() []CacheEntryInfo {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		util.LogModuleDebug("p2p", "[P2P] cache: read dir: %v", err)
		return nil
	}

	var result []CacheEntryInfo
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, ".sha256") {
			continue
		}
		entry, ok := parseCacheFileName(name)
		if !ok {
			continue
		}
		info := CacheEntryInfo{CacheEntry: entry}
		if fi, err := e.Info(); err == nil {
			info.Size = fi.Size()
			info.ModTime = fi.ModTime()
		}
		result = append(result, info)
	}
	return result
}

// parseCacheFileName extracts CacheEntry from a filename like
// "phaethon_linux_amd64_v1.2.3" or "phaethon_windows_amd64_win7_v1.2.3"
func parseCacheFileName(name string) (CacheEntry, bool) {
	if !strings.HasPrefix(name, "phaethon_") {
		return CacheEntry{}, false
	}
	rest := strings.TrimPrefix(name, "phaethon_")

	parts := strings.SplitN(rest, "_", 4)
	if len(parts) < 3 {
		return CacheEntry{}, false
	}

	platform := parts[0]
	arch := parts[1]

	var buildTag, version string
	if len(parts) == 3 {
		version = parts[2]
	} else {
		buildTag = parts[2]
		version = parts[3]
	}

	return CacheEntry{
		Platform: platform,
		Arch:     arch,
		Version:  version,
		BuildTag: buildTag,
	}, true
}

// maxCacheBinarySize bounds a single cached binary. Anything larger is
// rejected before it can fill the disk (D30).
const maxCacheBinarySize = 128 << 20 // 128 MiB

// writeSHA256Sidecar records the digest next to the cached file so the admin
// UI and later verification can compare without re-reading the binary.
func writeSHA256Sidecar(target, hashHex string) {
	if err := os.WriteFile(target+".sha256", []byte(hashHex+"\n"), 0644); err != nil {
		util.LogModuleWarn("p2p", "[P2P] cache: failed to write sha256 sidecar: %v", err)
	}
}

// StoreFromReader writes data from r into the cache as a new binary.
// Atomic rename avoids partial files, a unique temp name prevents two writers
// for the same key from clobbering each other, the size is capped, and the
// sha256 digest is recorded (D30).
func (c *BinaryCache) StoreFromReader(platform, arch, buildTag, version string, r io.Reader) (string, error) {
	target, err := c.FilePath(platform, arch, buildTag, version)
	if err != nil {
		return "", err
	}

	tmp, err := os.CreateTemp(c.dir, filepath.Base(target)+".*.tmp")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpPath)
	}

	hasher := sha256.New()
	n, err := io.Copy(tmp, io.TeeReader(io.LimitReader(r, maxCacheBinarySize+1), hasher))
	if err != nil {
		cleanup()
		return "", fmt.Errorf("write cache file: %w", err)
	}
	if n > maxCacheBinarySize {
		cleanup()
		return "", fmt.Errorf("cache: %s/%s/%s/%s exceeds the %d byte limit", platform, arch, buildTag, version, maxCacheBinarySize)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return "", fmt.Errorf("sync cache file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("close cache file: %w", err)
	}

	if err := os.Rename(tmpPath, target); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("rename to cache: %w", err)
	}
	writeSHA256Sidecar(target, hex.EncodeToString(hasher.Sum(nil)))

	util.LogModuleInfo("p2p", "[P2P] cache: stored %s/%s/%s/%s → %s", platform, arch, buildTag, version, filepath.Base(target))
	return target, nil
}

// Prune removes old cache entries. maxPerGroup keeps the newest N versions per
// platform/arch; maxTotalBytes caps the whole directory. The newest entry of
// every group is never deleted, so a node can always re-seed its own binary.
func (c *BinaryCache) Prune(maxPerGroup int, maxTotalBytes int64) int {
	entries := c.Inventory()
	if len(entries) == 0 {
		return 0
	}
	if maxPerGroup <= 0 {
		maxPerGroup = 1
	}
	groupKey := func(e CacheEntryInfo) string { return e.Platform + "/" + e.Arch }
	filePath := func(e CacheEntryInfo) string {
		p, err := c.FilePath(e.Platform, e.Arch, e.BuildTag, e.Version)
		if err != nil {
			return ""
		}
		return p
	}
	remove := func(e CacheEntryInfo) (int, int64) {
		p := filePath(e)
		if p == "" {
			return 0, 0
		}
		removed := 0
		if err := os.Remove(p); err == nil {
			removed = 1
		}
		_ = os.Remove(p + ".sha256")
		return removed, e.Size
	}

	groups := make(map[string][]CacheEntryInfo)
	for _, e := range entries {
		groups[groupKey(e)] = append(groups[groupKey(e)], e)
	}

	removed := 0
	newest := make(map[string]string, len(groups))
	var kept []CacheEntryInfo
	for g, list := range groups {
		sort.Slice(list, func(i, j int) bool {
			if cmp := CompareVersions(list[i].Version, list[j].Version); cmp != 0 {
				return cmp > 0
			}
			return list[i].ModTime.After(list[j].ModTime)
		})
		for i, e := range list {
			if i == 0 {
				newest[g] = filePath(e)
			}
			if i >= maxPerGroup {
				n, _ := remove(e)
				removed += n
				continue
			}
			kept = append(kept, e)
		}
	}

	if maxTotalBytes > 0 {
		var total int64
		for _, e := range kept {
			total += e.Size
		}
		if total > maxTotalBytes {
			sort.Slice(kept, func(i, j int) bool { return kept[i].ModTime.Before(kept[j].ModTime) })
			for _, e := range kept {
				if total <= maxTotalBytes {
					break
				}
				if newest[groupKey(e)] == filePath(e) {
					continue
				}
				n, size := remove(e)
				if n > 0 {
					total -= size
					removed += n
				}
			}
		}
	}

	if removed > 0 {
		util.LogModuleInfo("p2p", "[P2P] cache: pruned %d old entries", removed)
	}
	return removed
}

// CleanupBackup removes the .bak file next to the running executable.
func CleanupBackup() {
	execPath, err := os.Executable()
	if err != nil {
		return
	}
	bakPath := execPath + ".bak"
	if err := os.Remove(bakPath); err != nil && !os.IsNotExist(err) {
		util.LogModuleDebug("p2p", "[P2P] cleanup backup %s: %v", bakPath, err)
	} else if err == nil {
		util.LogModuleInfo("p2p", "[P2P] cleanup: removed %s", bakPath)
	}
}
