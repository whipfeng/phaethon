package admin

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"time"

	"phaethon/mesh"
	"phaethon/p2p"
	"phaethon/pkg/signing"
	"phaethon/util"
)

const (
	// packagesDir is where uploaded .pkg files are stored.
	packagesDir = "data/packages"

	defaultPackageUploadMaxMB = 100

	// packageRetention is the number of versions to keep per platform/arch.
	// Older versions are automatically deleted.
	packageRetention = 3
)

// versionPattern validates Semver format with strict completeness rules:
// - Base: v<major>.<minor>.<patch>[+<build>]
// - With commits: must have both -<commits> AND -g<hash>
// - Dirty: only allowed if commits info is present
// - Build metadata cannot contain dash (to avoid ambiguity with commits)
//
// Examples:
//
//	v1.0.0                                    ✓ base version
//	v1.0.0+mesh                               ✓ with build metadata
//	v1.0.0+mesh-150-g85a1766                  ✓ with commits (complete)
//	v1.0.0+mesh-150-g85a1766-dirty            ✓ with commits + dirty
//	v1.0.0-150-g85a1766                       ✓ without build, with commits
//	dev                                       ✓ dev build
//
// Invalid:
//
//	v1.0.0-mesh                               ✗ dash before build (use +)
//	v1.0.0+mesh-150                           ✗ commits without hash
//	v1.0.0+mesh-g85a1766                      ✗ hash without commits
//	v1.0.0-dirty                              ✗ dirty without commits
//	v1.0.0+mesh-build                         ✗ build cannot contain dash
var versionPattern = regexp.MustCompile(`^(dev|v[0-9]+\.[0-9]+\.[0-9]+(\+[a-zA-Z0-9._]+)?(-[0-9]+-g[0-9a-f]+(-dirty)?)?)$`)

var (
	packageIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// packageInfo is the metadata stored for each uploaded package.
type packageInfo struct {
	ID             string              `json:"id"`
	Filename       string              `json:"filename"`
	Size           int64               `json:"size"`
	UploadedAt     time.Time           `json:"uploadedAt"`
	SignatureValid bool                `json:"signatureValid"`
	Meta           signing.PackageMeta `json:"meta"`
	Published      bool                `json:"published"`
}

func (s *AdminServer) packageMaxUploadBytes() int64 {
	mb := defaultPackageUploadMaxMB
	if s.config != nil && s.config.PackageUploadMaxMB > 0 {
		mb = s.config.PackageUploadMaxMB
	}
	return int64(mb) * 1024 * 1024
}

// apiPackages handles GET /api/packages — list all packages.
func (s *AdminServer) apiPackages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	packages := s.listPackages()
	jsonResponse(w, map[string]interface{}{
		"packages": packages,
	})
}

// listPackages reads all package metadata files, newest first.
func (s *AdminServer) listPackages() []packageInfo {
	metas, err := filepath.Glob(filepath.Join(packagesDir, "*.json"))
	if err != nil {
		return []packageInfo{}
	}

	var result []packageInfo
	for _, metaPath := range metas {
		data, err := os.ReadFile(metaPath)
		if err != nil {
			continue
		}
		var info packageInfo
		if err := json.Unmarshal(data, &info); err != nil {
			continue
		}
		// Skip entries whose .pkg file vanished
		if _, err := os.Stat(filepath.Join(packagesDir, info.ID+".pkg")); err != nil {
			os.Remove(metaPath)
			continue
		}
		result = append(result, info)
	}

	// Newest first
	for i := 0; i < len(result); i++ {
		for j := i + 1; j < len(result); j++ {
			if result[j].UploadedAt.After(result[i].UploadedAt) {
				result[i], result[j] = result[j], result[i]
			}
		}
	}
	return result
}

// apiPackageUpload handles POST /api/packages/upload — multipart .pkg upload.
func (s *AdminServer) apiPackageUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse multipart form (max size limited)
	if err := r.ParseMultipartForm(s.packageMaxUploadBytes() + 1024); err != nil {
		httpError(w, "parse multipart: "+err.Error(), http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		httpError(w, "get file: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer file.Close()

	// Check size
	if header.Size > s.packageMaxUploadBytes() {
		httpError(w, fmt.Sprintf("package exceeds size limit (%d MB)", s.packageMaxUploadBytes()/1024/1024), http.StatusRequestEntityTooLarge)
		return
	}

	// Create packages dir
	if err := os.MkdirAll(packagesDir, 0755); err != nil {
		httpError(w, "create packages dir: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Generate ID
	idBytes := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, idBytes); err != nil {
		httpError(w, "generate id: "+err.Error(), http.StatusInternalServerError)
		return
	}
	id := hex.EncodeToString(idBytes)

	// Save to temp file
	tmpPath := filepath.Join(packagesDir, id+".pkg.tmp")
	tmpFile, err := os.Create(tmpPath)
	if err != nil {
		httpError(w, "create temp file: "+err.Error(), http.StatusInternalServerError)
		return
	}

	size, err := io.Copy(tmpFile, file)
	tmpFile.Close()
	if err != nil {
		os.Remove(tmpPath)
		httpError(w, "save upload: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Verify signature
	contents, err := signing.VerifyPkg(tmpPath, signing.PublicKey)
	if err != nil {
		os.Remove(tmpPath)
		httpError(w, "signature verification failed: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Validate version format
	if !versionPattern.MatchString(contents.Meta.Version) {
		os.Remove(tmpPath)
		httpError(w, fmt.Sprintf("invalid version format: %s (expected: v<major>.<minor>.<patch>[+<build>])", contents.Meta.Version), http.StatusBadRequest)
		return
	}

	// Rename to final path
	pkgPath := filepath.Join(packagesDir, id+".pkg")
	if err := os.Rename(tmpPath, pkgPath); err != nil {
		os.Remove(tmpPath)
		httpError(w, "finalize upload: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Save metadata
	info := packageInfo{
		ID:             id,
		Filename:       header.Filename,
		Size:           size,
		UploadedAt:     time.Now(),
		SignatureValid: true,
		Meta:           contents.Meta,
		Published:      false,
	}
	if err := s.savePackageMeta(info); err != nil {
		os.Remove(pkgPath)
		httpError(w, "save metadata: "+err.Error(), http.StatusInternalServerError)
		return
	}

	util.LogInfo("[ADMIN] package uploaded: %s (version=%s, platform=%s/%s)", header.Filename, contents.Meta.Version, contents.Meta.Platform, contents.Meta.Arch)
	util.DefaultVersionNotifier.BumpVersion("packages")

	// Distribute to mesh peers
	if pkgData, err := os.ReadFile(pkgPath); err == nil {
		util.LogInfo("[ADMIN] read pkg for distribution: %d bytes", len(pkgData))
		go s.DistributePackage(pkgData)
	} else {
		util.LogInfo("[ADMIN] failed to read pkg for distribution: %v", err)
	}

	// Check if there's a newer version, exit if so
	go s.checkForNewerVersion(contents)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(info)
}

// savePackageMeta persists package metadata as JSON.
func (s *AdminServer) savePackageMeta(info packageInfo) error {
	data, err := json.Marshal(info)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(packagesDir, info.ID+".json"), data, 0644)
}

// loadPackageMeta reads package metadata by id.
func (s *AdminServer) loadPackageMeta(id string) (*packageInfo, error) {
	if !packageIDPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid package id")
	}
	data, err := os.ReadFile(filepath.Join(packagesDir, id+".json"))
	if err != nil {
		return nil, fmt.Errorf("package not found")
	}
	var info packageInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("corrupt package metadata")
	}
	return &info, nil
}

// apiPackageDownload handles GET /api/packages/{id}/download.
func (s *AdminServer) apiPackageDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract ID from path: /api/packages/{id}/download
	path := r.URL.Path
	rest := path[len("/api/packages/"):]
	parts := splitPath(rest)
	if len(parts) < 2 {
		httpError(w, "missing package id", http.StatusBadRequest)
		return
	}
	id := parts[0]

	info, err := s.loadPackageMeta(id)
	if err != nil {
		httpError(w, err.Error(), http.StatusNotFound)
		return
	}

	pkgPath := filepath.Join(packagesDir, id+".pkg")
	if _, err := os.Stat(pkgPath); err != nil {
		httpError(w, "package file missing", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, info.Filename))
	http.ServeFile(w, r, pkgPath)
}

// apiPackagePublish handles POST /api/packages/{id}/publish — mark as published.
// Version monotonic: the version must be higher than the current latest for the same platform/arch.
func (s *AdminServer) apiPackagePublish(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract ID from path: /api/packages/{id}/publish
	path := r.URL.Path
	rest := path[len("/api/packages/"):]
	parts := splitPath(rest)
	if len(parts) < 2 {
		httpError(w, "missing package id", http.StatusBadRequest)
		return
	}
	id := parts[0]

	info, err := s.loadPackageMeta(id)
	if err != nil {
		httpError(w, err.Error(), http.StatusNotFound)
		return
	}

	// Version monotonic check: must be higher than current latest for same platform/arch
	packages := s.listPackages()
	platform := info.Meta.Platform
	arch := info.Meta.Arch
	newVersion := info.Meta.Version

	// Find current latest version for this platform/arch
	var currentLatest string
	for _, pkg := range packages {
		if pkg.Meta.Platform == platform && pkg.Meta.Arch == arch && pkg.Published {
			if currentLatest == "" || p2p.CompareVersions(pkg.Meta.Version, currentLatest) > 0 {
				currentLatest = pkg.Meta.Version
			}
		}
	}

	// Check if new version is higher
	if currentLatest != "" && p2p.CompareVersions(newVersion, currentLatest) <= 0 {
		httpError(w, fmt.Sprintf("version must be higher than current latest (%s)", currentLatest), http.StatusBadRequest)
		return
	}

	// Mark this as published
	info.Published = true
	if err := s.savePackageMeta(*info); err != nil {
		httpError(w, "update metadata: "+err.Error(), http.StatusInternalServerError)
		return
	}

	util.LogInfo("[ADMIN] package published: %s (version=%s platform=%s arch=%s)", id, info.Meta.Version, platform, arch)
	util.DefaultVersionNotifier.BumpVersion("packages")

	// Read the package file and distribute to mesh peers
	pkgPath := filepath.Join(packagesDir, id+".pkg")
	if pkgData, err := os.ReadFile(pkgPath); err == nil {
		go s.DistributePackage(pkgData)
		// Apply retention after distribution
		go s.applyRetention(platform, arch)
	}

	jsonResponse(w, map[string]interface{}{
		"status": "published",
		"id":     id,
	})
}

// apiPackageDelete handles DELETE /api/packages/{id}.
func (s *AdminServer) apiPackageDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id := filepath.Base(r.URL.Path)

	if _, err := s.loadPackageMeta(id); err != nil {
		httpError(w, err.Error(), http.StatusNotFound)
		return
	}

	os.Remove(filepath.Join(packagesDir, id+".pkg"))
	os.Remove(filepath.Join(packagesDir, id+".json"))
	util.LogInfo("[ADMIN] package deleted: %s", id)
	util.DefaultVersionNotifier.BumpVersion("packages")
	jsonResponse(w, map[string]string{"status": "deleted"})
}

// apiPackageItem routes /api/packages/{id}/* to the appropriate handler.
func (s *AdminServer) apiPackageItem(w http.ResponseWriter, r *http.Request) {
	// Extract the part after /api/packages/
	path := r.URL.Path
	rest := path[len("/api/packages/"):]

	// Check for suffixes
	if rest == "" {
		httpError(w, "missing package id", http.StatusBadRequest)
		return
	}

	// Split by /
	parts := splitPath(rest)
	if len(parts) == 0 {
		httpError(w, "missing package id", http.StatusBadRequest)
		return
	}

	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch action {
	case "download":
		s.apiPackageDownload(w, r)
	case "publish":
		s.apiPackagePublish(w, r)
	case "":
		// DELETE /api/packages/{id}
		s.apiPackageDelete(w, r)
	default:
		httpError(w, "unknown action: "+action, http.StatusNotFound)
	}
}

// splitPath splits a path by / and filters empty segments.
func splitPath(path string) []string {
	var result []string
	for _, seg := range splitString(path, '/') {
		if seg != "" {
			result = append(result, seg)
		}
	}
	return result
}

// splitString splits a string by a separator.
func splitString(s string, sep byte) []string {
	var result []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			if i > start {
				result = append(result, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		result = append(result, s[start:])
	}
	return result
}

// apiPackageReceive handles POST /api/packages/receive — receive package from mesh peer.
func (s *AdminServer) apiPackageReceive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Read body (.pkg file)
	pkgData, err := io.ReadAll(r.Body)
	if err != nil {
		httpError(w, "read body failed", http.StatusBadRequest)
		return
	}

	// Verify signature
	contents, err := signing.VerifyPkgBytes(pkgData, signing.PublicKey)
	if err != nil {
		util.LogDebug("[ADMIN] receive package verify failed: %v", err)
		httpError(w, "verify failed", http.StatusBadRequest)
		return
	}

	// Uniqueness is (platform, arch, version) — not version alone, and not the
	// ID, which is a random local filename that differs across nodes.
	packages := s.listPackages()
	for _, p := range packages {
		if p.Meta.Platform == contents.Meta.Platform && p.Meta.Arch == contents.Meta.Arch && p.Meta.Version == contents.Meta.Version {
			util.LogDebug("[ADMIN] receive package: already have %s/%s@%s", p.Meta.Platform, p.Meta.Arch, p.Meta.Version)
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	// Save the package
	id, err := s.saveExternalPackage(pkgData, contents)
	if err != nil {
		util.LogDebug("[ADMIN] receive package save failed: %v", err)
		httpError(w, "save failed", http.StatusInternalServerError)
		return
	}

	util.LogInfo("[ADMIN] received package from mesh peer: %s (version=%s)", id, contents.Meta.Version)
	util.DefaultVersionNotifier.BumpVersion("packages")

	// Continue distributing to other peers (flood fill)
	go s.DistributePackage(pkgData)

	w.WriteHeader(http.StatusOK)
}

// saveExternalPackage saves a package received from a mesh peer.
func (s *AdminServer) saveExternalPackage(pkgData []byte, contents *signing.PkgContents) (string, error) {
	if err := os.MkdirAll(packagesDir, 0755); err != nil {
		return "", fmt.Errorf("create packages dir: %w", err)
	}

	// Random local file ID — cross-node uniqueness is (platform, arch, version)
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return "", err
	}
	id := hex.EncodeToString(idBytes)

	// Save .pkg file
	pkgPath := filepath.Join(packagesDir, id+".pkg")
	if err := os.WriteFile(pkgPath, pkgData, 0644); err != nil {
		return "", err
	}

	// Save metadata
	info := packageInfo{
		ID:             id,
		Filename:       fmt.Sprintf("phaethon-%s.pkg", contents.Meta.Version),
		Size:           int64(len(pkgData)),
		UploadedAt:     time.Now(),
		SignatureValid: true,
		Meta:           contents.Meta,
		Published:      true, // Auto-publish received packages
	}

	if err := s.savePackageMeta(info); err != nil {
		os.Remove(pkgPath)
		return "", err
	}

	return id, nil
}

// DistributePackage distributes a package to all connected mesh peers.
func (s *AdminServer) DistributePackage(pkgData []byte) {
	if s.peerLister == nil {
		util.LogInfo("[ADMIN] distribute: peerLister is nil")
		return
	}
	if s.meshHTTPClient == nil {
		util.LogInfo("[ADMIN] distribute: meshHTTPClient is nil")
		return
	}

	peers := s.peerLister()
	if len(peers) == 0 {
		util.LogInfo("[ADMIN] distribute: no peers")
		return
	}

	util.LogInfo("[ADMIN] distributing package to %d peers", len(peers))

	for _, peer := range peers {
		go func(nodeID string) {
			domain := mesh.NodeDomain(nodeID)
			url := fmt.Sprintf("https://%s/api/packages/receive", domain)
			resp, err := s.meshHTTPClient.Post(url, "application/octet-stream", io.NopCloser(io.NewSectionReader(newBytesReaderAt(pkgData), 0, int64(len(pkgData)))))
			if err != nil {
				util.LogInfo("[ADMIN] distribute package to %s (%s) failed: %v", nodeID, domain, err)
				return
			}
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				util.LogInfo("[ADMIN] distribute package to %s (%s) rejected: status=%d body=%s", nodeID, domain, resp.StatusCode, string(body))
				return
			}
			util.LogInfo("[ADMIN] distributed package to %s (%s), status=%d", nodeID, domain, resp.StatusCode)
		}(peer.NodeID)
	}
}

// bytesReaderAt wraps a byte slice to implement io.ReaderAt.
type bytesReaderAt struct {
	data []byte
}

func newBytesReaderAt(data []byte) *bytesReaderAt {
	return &bytesReaderAt{data: data}
}

func (r *bytesReaderAt) ReadAt(p []byte, off int64) (n int, err error) {
	if off >= int64(len(r.data)) {
		return 0, io.EOF
	}
	n = copy(p, r.data[off:])
	if n < len(p) {
		err = io.EOF
	}
	return
}

// pkgKey is the package uniqueness key: platform/arch@version. IDs are random
// local filenames and differ across nodes, so dedup must never compare by ID.
func pkgKey(meta signing.PackageMeta) string {
	return meta.Platform + "/" + meta.Arch + "@" + meta.Version
}

// SyncFromPeer syncs packages from a specific mesh peer (called when peer connects).
func (s *AdminServer) SyncFromPeer(nodeID string) {
	if s.meshHTTPClient == nil {
		return
	}
	// Small delay to ensure routing is established
	time.Sleep(2 * time.Second)
	s.syncFromPeer(nodeID)
}

func (s *AdminServer) syncFromPeer(nodeID string) {
	domain := mesh.NodeDomain(nodeID)
	url := fmt.Sprintf("https://%s/api/packages", domain)
	resp, err := s.meshHTTPClient.Get(url)
	if err != nil {
		util.LogDebug("[ADMIN] sync from %s failed: %v", nodeID, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		util.LogDebug("[ADMIN] sync from %s failed: status %d", nodeID, resp.StatusCode)
		return
	}

	var result struct {
		Packages []packageInfo `json:"packages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		util.LogDebug("[ADMIN] sync from %s decode failed: %v", nodeID, err)
		return
	}

	// Build the retention set: per platform/arch, merge the peer's list with
	// the local list, sort by version descending, keep the top N. Deletion
	// must be based on the merged set — a peer with an empty (or partial)
	// list must never wipe packages synced from other peers.
	type platformArch struct {
		platform string
		arch     string
	}
	groups := make(map[platformArch][]packageInfo)
	for _, pkg := range result.Packages {
		if !pkg.Published {
			continue
		}
		key := platformArch{pkg.Meta.Platform, pkg.Meta.Arch}
		groups[key] = append(groups[key], pkg)
	}

	// Get local packages
	localPackages := s.listPackages()
	localKeys := make(map[string]bool) // pkgKey → have
	for _, p := range localPackages {
		localKeys[pkgKey(p.Meta)] = true
		groups[platformArch{p.Meta.Platform, p.Meta.Arch}] = append(groups[platformArch{p.Meta.Platform, p.Meta.Arch}], p)
	}

	// Per group, sort by version descending and take top N
	targetKeys := make(map[string]bool) // pkgKey → should have
	for _, pkgs := range groups {
		sort.Slice(pkgs, func(i, j int) bool {
			return p2p.CompareVersions(pkgs[i].Meta.Version, pkgs[j].Meta.Version) > 0
		})
		limit := packageRetention
		if limit > len(pkgs) {
			limit = len(pkgs)
		}
		for _, pkg := range pkgs[:limit] {
			targetKeys[pkgKey(pkg.Meta)] = true
		}
	}

	// Download packages in target set that we don't have
	downloadSuccess := true
	for _, pkg := range result.Packages {
		key := pkgKey(pkg.Meta)
		if !targetKeys[key] {
			continue
		}
		if localKeys[key] {
			continue
		}

		// Download the package
		downloadURL := fmt.Sprintf("https://%s/api/packages/%s/download", domain, pkg.ID)
		dlResp, err := s.meshHTTPClient.Get(downloadURL)
		if err != nil {
			util.LogDebug("[ADMIN] download package %s from %s failed: %v", pkg.ID, nodeID, err)
			downloadSuccess = false
			continue
		}

		pkgData, err := io.ReadAll(dlResp.Body)
		dlResp.Body.Close()
		if err != nil {
			util.LogDebug("[ADMIN] download package %s from %s read failed: %v", pkg.ID, nodeID, err)
			downloadSuccess = false
			continue
		}

		// Verify and save
		contents, err := signing.VerifyPkgBytes(pkgData, signing.PublicKey)
		if err != nil {
			util.LogDebug("[ADMIN] download package %s from %s verify failed: %v", pkg.ID, nodeID, err)
			downloadSuccess = false
			continue
		}

		id, err := s.saveExternalPackage(pkgData, contents)
		if err != nil {
			util.LogDebug("[ADMIN] download package %s from %s save failed: %v", pkg.ID, nodeID, err)
			downloadSuccess = false
			continue
		}

		localKeys[key] = true
		util.LogInfo("[ADMIN] synced package from %s: %s (version=%s)", nodeID, id, contents.Meta.Version)
		util.DefaultVersionNotifier.BumpVersion("packages")

		// Check if there's a newer version, exit if so
		go s.checkForNewerVersion(contents)
	}

	// Only delete old versions if all downloads succeeded
	if downloadSuccess {
		// Delete local packages not in the target set; a pkgKey in the target
		// set keeps exactly one copy (extra copies are duplicates left by the
		// old ID-based comparison).
		keptKeys := make(map[string]bool)
		for _, pkg := range localPackages {
			key := pkgKey(pkg.Meta)
			if targetKeys[key] && !keptKeys[key] {
				keptKeys[key] = true
				continue
			}
			util.LogInfo("[ADMIN] retention: deleting %s (id=%s)", key, pkg.ID)
			os.Remove(filepath.Join(packagesDir, pkg.ID+".pkg"))
			os.Remove(filepath.Join(packagesDir, pkg.ID+".json"))
		}
		if len(localPackages) > 0 {
			util.DefaultVersionNotifier.BumpVersion("packages")
		}
	}
}

// applyAllRetention applies retention policy to all platform/arch combinations.
func (s *AdminServer) applyAllRetention() {
	packages := s.listPackages()

	// Group by platform/arch
	type platformArch struct {
		platform string
		arch     string
	}
	groups := make(map[platformArch][]packageInfo)
	for _, pkg := range packages {
		key := platformArch{pkg.Meta.Platform, pkg.Meta.Arch}
		groups[key] = append(groups[key], pkg)
	}

	// Apply retention to each group
	for key, pkgs := range groups {
		s.applyRetentionForGroup(pkgs, key.platform, key.arch)
	}
}

// applyRetention applies retention policy to a specific platform/arch combination.
func (s *AdminServer) applyRetention(platform, arch string) {
	packages := s.listPackages()
	var group []packageInfo
	for _, pkg := range packages {
		if pkg.Meta.Platform == platform && pkg.Meta.Arch == arch {
			group = append(group, pkg)
		}
	}
	s.applyRetentionForGroup(group, platform, arch)
}

// applyRetentionForGroup applies retention to a group of packages for the same platform/arch.
// Keeps the latest packageRetention versions, deletes the rest.
func (s *AdminServer) applyRetentionForGroup(pkgs []packageInfo, platform, arch string) {
	if len(pkgs) <= packageRetention {
		return
	}

	// Sort by version descending (newest first)
	sort.Slice(pkgs, func(i, j int) bool {
		return p2p.CompareVersions(pkgs[i].Meta.Version, pkgs[j].Meta.Version) > 0
	})

	// Delete packages beyond retention limit
	toDelete := pkgs[packageRetention:]
	for _, pkg := range toDelete {
		util.LogInfo("[ADMIN] retention: deleting %s/%s version %s (id=%s)", platform, arch, pkg.Meta.Version, pkg.ID)
		os.Remove(filepath.Join(packagesDir, pkg.ID+".pkg"))
		os.Remove(filepath.Join(packagesDir, pkg.ID+".json"))
	}
	util.DefaultVersionNotifier.BumpVersion("packages")
}

// checkForNewerVersion checks if the package version is higher than current.
// If so, exits the process so watchdog can restart with the new version.
func (s *AdminServer) checkForNewerVersion(contents *signing.PkgContents) {
	// 1. Check if platform/arch matches
	if contents.Meta.Platform != runtime.GOOS || contents.Meta.Arch != runtime.GOARCH {
		util.LogInfo("[ADMIN] checkForNewerVersion: platform/arch mismatch (current=%s/%s)",
			runtime.GOOS, runtime.GOARCH)
		return
	}

	// 2. Check if version is higher
	if s.GetCurrentVersion == nil {
		util.LogInfo("[ADMIN] checkForNewerVersion: GetCurrentVersion is nil")
		return
	}
	currentVersion := s.GetCurrentVersion()
	util.LogInfo("[ADMIN] checkForNewerVersion: current version=%s", currentVersion)
	if p2p.CompareVersions(contents.Meta.Version, currentVersion) <= 0 {
		util.LogInfo("[ADMIN] checkForNewerVersion: version not higher")
		return
	}

	util.LogInfo("[ADMIN] detected newer version %s > current %s, exiting for watchdog restart...",
		contents.Meta.Version, currentVersion)

	// 3. Exit immediately, watchdog will restart with new version
	// Use os.Exit(0) instead of SIGTERM for immediate exit (no graceful shutdown)
	// This is safe because we're about to restart anyway
	go func() {
		time.Sleep(1 * time.Second)
		os.Exit(0)
	}()
}
