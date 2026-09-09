package p2p

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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
	dir := filepath.Join(workdir, "p2p-cache")
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
func (c *BinaryCache) SeedOwnBinary(version, platform, arch, buildTag string) error {
	target, err := c.FilePath(platform, arch, buildTag, version)
	if err != nil {
		return err
	}
	if _, err := os.Stat(target); err == nil {
		util.LogDebug("[P2P] cache: own binary already cached: %s", filepath.Base(target))
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

	tmpPath := target + ".tmp"
	dst, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}

	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("copy binary: %w", err)
	}
	dst.Close()

	if err := os.Rename(tmpPath, target); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename to cache: %w", err)
	}

	util.LogInfo("[P2P] cache: seeded own binary: %s", filepath.Base(target))
	return nil
}

// ListInventory scans the cache directory and returns all entries.
func (c *BinaryCache) ListInventory() []CacheEntry {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		util.LogDebug("[P2P] cache: read dir: %v", err)
		return nil
	}

	var result []CacheEntry
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".tmp") {
			continue
		}
		entry, ok := parseCacheFileName(name)
		if !ok {
			continue
		}
		result = append(result, entry)
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

// StoreFromReader writes data from r into the cache as a new binary.
// Uses atomic rename to avoid partial files.
func (c *BinaryCache) StoreFromReader(platform, arch, buildTag, version string, r io.Reader) (string, error) {
	target, err := c.FilePath(platform, arch, buildTag, version)
	if err != nil {
		return "", err
	}
	tmpPath := target + ".tmp"

	dst, err := os.Create(tmpPath)
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}

	if _, err := io.Copy(dst, r); err != nil {
		dst.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("write cache file: %w", err)
	}
	dst.Close()

	if err := os.Rename(tmpPath, target); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("rename to cache: %w", err)
	}

	util.LogInfo("[P2P] cache: stored %s/%s/%s/%s → %s", platform, arch, buildTag, version, filepath.Base(target))
	return target, nil
}

// RemoveEntry deletes a specific entry from the cache.
func (c *BinaryCache) RemoveEntry(platform, arch, buildTag, version string) error {
	path, err := c.FilePath(platform, arch, buildTag, version)
	if err != nil {
		return err
	}
	return os.Remove(path)
}

// HasEntry checks if a specific entry exists in the cache.
func (c *BinaryCache) HasEntry(platform, arch, buildTag, version string) bool {
	path, err := c.FilePath(platform, arch, buildTag, version)
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// CleanupBackup removes the .bak file next to the running executable.
func CleanupBackup() {
	execPath, err := os.Executable()
	if err != nil {
		return
	}
	bakPath := execPath + ".bak"
	if err := os.Remove(bakPath); err != nil && !os.IsNotExist(err) {
		util.LogDebug("[P2P] cleanup backup %s: %v", bakPath, err)
	} else if err == nil {
		util.LogInfo("[P2P] cleanup: removed %s", bakPath)
	}
}
