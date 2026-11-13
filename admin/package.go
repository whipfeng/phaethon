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
	"sort"
	"sync"
	"time"

	"phaethon/db"
	"phaethon/mesh"
	"phaethon/p2p"
	"phaethon/pkg/signing"
	"phaethon/pkg/upgrade"
	"phaethon/util"
)

const (
	// packagesDir is where activated .pkg files are stored. The watchdog scans
	// only this directory, so packages here can change the running version.
	packagesDir = "data/packages"

	// pendingPackagesDir holds uploaded/received packages that have not been
	// activated yet. The watchdog never scans it; an administrator must
	// explicitly activate a version before it can take effect.
	pendingPackagesDir = "data/packages-pending"

	// workerPinPath is the legacy pin written by older builds. The watchdog
	// ignores it now; clearWorkerPin removes leftovers.
	workerPinPath = "data/state/worker-pin"

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

// listPackages reads all package metadata from database, newest first.
func (s *AdminServer) listPackages() []*db.PackageMeta {
	packages, err := db.ListPackages()
	if err != nil {
		util.LogModuleWarn("admin", "list packages from database failed: %v", err)
		return []*db.PackageMeta{}
	}

	// Filter out entries whose .pkg file vanished
	var result []*db.PackageMeta
	for _, pkg := range packages {
		if _, ok := pkgPathFor(pkg.ID); ok {
			result = append(result, pkg)
		} else {
			// Clean up orphaned metadata
			_ = db.DeletePackage(pkg.ID)
		}
	}
	return result
}

// pkgPathFor returns the on-disk path of a package, checking the active
// directory first and then the pending directory.
func pkgPathFor(id string) (string, bool) {
	for _, dir := range []string{packagesDir, pendingPackagesDir} {
		p := filepath.Join(dir, id+".pkg")
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}

// clearWorkerPin removes the legacy pin so the watchdog does not log a warning
// about a pin it no longer honours.
func clearWorkerPin() error {
	err := os.Remove(workerPinPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// apiPackagePin handles GET/DELETE /api/packages/pin.
//
// The name is kept for API compatibility; it now reports the self-replacement
// upgrade state requested by activate. GET returns {current, requested,
// status, lastError}; DELETE cancels a pending (not yet applying) request.
func (s *AdminServer) apiPackagePin(w http.ResponseWriter, r *http.Request) {
	current := ""
	if s.GetCurrentVersion != nil {
		current = s.GetCurrentVersion()
	}
	switch r.Method {
	case http.MethodGet:
		st := upgrade.ReadStatus()
		requested := st.Requested
		if req := upgrade.ReadRequest(); req != nil {
			requested = req.Version
		}
		jsonResponse(w, map[string]interface{}{
			"current":   current,
			"requested": requested,
			"status":    st.Status,
			"lastError": st.LastError,
			"updatedAt": st.UpdatedAt,
		})
	case http.MethodDelete, http.MethodPost:
		if upgrade.ReadStatus().Status == upgrade.StatusApplying {
			httpError(w, "upgrade is applying; cannot cancel", http.StatusConflict)
			return
		}
		_ = upgrade.ClearRequest()
		_ = clearWorkerPin() // legacy pin: ignored by the watchdog
		_ = upgrade.WriteStatus(upgrade.Status{Status: upgrade.StatusIdle, Current: current})
		util.LogModuleInfo("admin", "pending upgrade cancelled (current %q)", current)
		jsonResponse(w, map[string]interface{}{
			"status":  upgrade.StatusIdle,
			"current": current,
		})
	default:
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
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

	// Uploads land in the pending directory: they never change the running
	// version until an administrator explicitly activates one.
	if err := os.MkdirAll(pendingPackagesDir, 0755); err != nil {
		httpError(w, "create pending dir: "+err.Error(), http.StatusInternalServerError)
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
	tmpPath := filepath.Join(pendingPackagesDir, id+".pkg.tmp")
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

	// Rename to final path (pending until activated)
	pkgPath := filepath.Join(pendingPackagesDir, id+".pkg")
	if err := os.Rename(tmpPath, pkgPath); err != nil {
		os.Remove(tmpPath)
		httpError(w, "finalize upload: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Save metadata
	info := &db.PackageMeta{
		ID:             id,
		Filename:       header.Filename,
		Size:           size,
		UploadedAt:     time.Now(),
		SignatureValid: true,
		Version:        contents.Meta.Version,
		Platform:       contents.Meta.Platform,
		Arch:           contents.Meta.Arch,
		BuildTime:      contents.Meta.BuildTime,
		GitCommit:      contents.Meta.GitCommit,
		GoVersion:      contents.Meta.GoVersion,
		BuildTag:       contents.Meta.BuildTag,
		Published:      false,
	}
	if err := db.PutPackage(info); err != nil {
		os.Remove(pkgPath)
		httpError(w, "save metadata: "+err.Error(), http.StatusInternalServerError)
		return
	}

	util.LogModuleInfo("admin", "package uploaded: %s (version=%s, platform=%s/%s)", header.Filename, info.Version, info.Platform, info.Arch)

	// Stored as pending only: no automatic upgrade, no automatic distribution.
	util.LogModuleInfo("admin", "package stored as pending: version=%s (activate to apply)", info.Version)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(info)
}

// loadPackageMeta reads package metadata by id from database.
func (s *AdminServer) loadPackageMeta(id string) (*db.PackageMeta, error) {
	if !packageIDPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid package id")
	}
	pkg, err := db.GetPackage(id)
	if err != nil {
		return nil, fmt.Errorf("package not found")
	}
	return pkg, nil
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

	pkgPath, ok := pkgPathFor(id)
	if !ok {
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

	// A second activation while the watchdog is mid-swap would be lost when
	// the process restarts; reject it instead of silently overwriting.
	if st := upgrade.ReadStatus(); st.Status == upgrade.StatusApplying {
		httpError(w, "an upgrade is already applying", http.StatusConflict)
		return
	}

	// Activation is explicit and allows any downloaded version, including an
	// older one (rollback). No monotonic restriction.
	platform := info.Platform
	arch := info.Arch

	// Move the package from pending to active; the watchdog scans only active.
	if p, ok := pkgPathFor(id); ok {
		activePath := filepath.Join(packagesDir, id+".pkg")
		if p != activePath {
			if err := os.MkdirAll(packagesDir, 0755); err != nil {
				httpError(w, "create packages dir: "+err.Error(), http.StatusInternalServerError)
				return
			}
			if err := os.Rename(p, activePath); err != nil {
				httpError(w, "activate package: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
	} else {
		httpError(w, "package file missing", http.StatusNotFound)
		return
	}

	info.Published = true
	if err := db.PutPackage(info); err != nil {
		httpError(w, "update metadata: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// The legacy pin path is gone: the watchdog always runs the stable
	// executable and applies upgrades through data/state/upgrade-request.
	_ = clearWorkerPin()

	util.LogModuleInfo("admin", "package activated: %s (version=%s platform=%s arch=%s)", id, info.Version, platform, arch)

	// Distribution is an explicit admin action, performed only on activation.
	// It completes (bounded) before a local upgrade request is written: the
	// watchdog stops this process within seconds, which would otherwise abort
	// in-flight peer uploads.
	if pkgData, err := os.ReadFile(filepath.Join(packagesDir, id+".pkg")); err == nil {
		s.distributePackage(pkgData, 60*time.Second)
		// Run retention synchronously before writing the request. Async
		// retention used to be cut off by the self-upgrade, and the activated
		// (possibly older rollback) version must be explicitly protected.
		s.applyRetention(platform, arch, info.Version)
	}

	localPlatform, localArch := "", ""
	if s.GetPlatform != nil {
		localPlatform = s.GetPlatform()
	}
	if s.GetArch != nil {
		localArch = s.GetArch()
	}
	localMatch := localPlatform != "" && localArch != "" &&
		info.Platform == localPlatform && info.Arch == localArch

	if localMatch {
		if err := upgrade.WriteRequest(upgrade.Request{Version: info.Version, PackageID: id}); err != nil {
			httpError(w, "write upgrade request: "+err.Error(), http.StatusInternalServerError)
			return
		}
		util.LogModuleInfo("admin", "upgrade requested: version=%s (watchdog applies within ~2s)", info.Version)
	} else {
		util.LogModuleInfo("admin", "package activated for distribution only: %s/%s does not match local %s/%s",
			info.Platform, info.Arch, localPlatform, localArch)
	}

	jsonResponse(w, map[string]interface{}{
		"status":       "requested",
		"id":           id,
		"version":      info.Version,
		"localUpgrade": localMatch,
	})
}

// apiPackageDelete handles DELETE /api/packages/{id}.
func (s *AdminServer) apiPackageDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id := filepath.Base(r.URL.Path)

	meta, err := s.loadPackageMeta(id)
	if err != nil {
		httpError(w, err.Error(), http.StatusNotFound)
		return
	}

	if p, ok := pkgPathFor(id); ok {
		os.Remove(p)
	}
	_ = db.DeletePackage(id)
	// A pending request pointing at the deleted version would fail on the
	// watchdog side; clear it here and leave the running worker alone.
	if meta.Version != "" {
		if req := upgrade.ReadRequest(); req != nil && req.Version == meta.Version {
			_ = upgrade.ClearRequest()
			util.LogModuleWarn("admin", "pending upgrade request cleared because package %s (%s) was deleted", id, meta.Version)
		}
	}
	_ = clearWorkerPin() // legacy pin: ignored by the watchdog
	util.LogModuleInfo("admin", "package deleted: %s", id)
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
	case "publish", "activate":
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

	// Read body (.pkg file), bounded so a peer cannot exhaust memory.
	r.Body = http.MaxBytesReader(w, r.Body, s.packageMaxUploadBytes())
	pkgData, err := io.ReadAll(r.Body)
	if err != nil {
		httpError(w, "read body failed", http.StatusBadRequest)
		return
	}

	// Verify signature
	contents, err := signing.VerifyPkgBytes(pkgData, signing.PublicKey)
	if err != nil {
		util.LogModuleDebug("admin", "receive package verify failed: %v", err)
		httpError(w, "verify failed", http.StatusBadRequest)
		return
	}

	// Uniqueness is (platform, arch, version) — not version alone, and not the
	// ID, which is a random local filename that differs across nodes.
	packages := s.listPackages()
	for _, p := range packages {
		if p.Platform == contents.Meta.Platform && p.Arch == contents.Meta.Arch && p.Version == contents.Meta.Version {
			util.LogModuleDebug("admin", "receive package: already have %s/%s@%s", p.Platform, p.Arch, p.Version)
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	// Save the package
	id, err := s.saveExternalPackage(pkgData, contents)
	if err != nil {
		util.LogModuleDebug("admin", "receive package save failed: %v", err)
		httpError(w, "save failed", http.StatusInternalServerError)
		return
	}

	util.LogModuleInfo("admin", "received package from mesh peer: %s (version=%s)", id, contents.Meta.Version)

	// Received packages stay pending: no automatic upgrade and no automatic
	// forwarding. An administrator decides whether to activate or distribute.

	w.WriteHeader(http.StatusOK)
}

// saveExternalPackage saves a package received from a mesh peer.
func (s *AdminServer) saveExternalPackage(pkgData []byte, contents *signing.PkgContents) (string, error) {
	if err := os.MkdirAll(pendingPackagesDir, 0755); err != nil {
		return "", fmt.Errorf("create pending dir: %w", err)
	}

	// Random local file ID — cross-node uniqueness is (platform, arch, version)
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return "", err
	}
	id := hex.EncodeToString(idBytes)

	// Save .pkg file (pending until activated)
	pkgPath := filepath.Join(pendingPackagesDir, id+".pkg")
	if err := os.WriteFile(pkgPath, pkgData, 0644); err != nil {
		return "", err
	}

	// Save metadata
	info := &db.PackageMeta{
		ID:             id,
		Filename:       fmt.Sprintf("phaethon-%s.pkg", contents.Meta.Version),
		Size:           int64(len(pkgData)),
		UploadedAt:     time.Now(),
		SignatureValid: true,
		Version:        contents.Meta.Version,
		Platform:       contents.Meta.Platform,
		Arch:           contents.Meta.Arch,
		BuildTime:      contents.Meta.BuildTime,
		GitCommit:      contents.Meta.GitCommit,
		GoVersion:      contents.Meta.GoVersion,
		BuildTag:       contents.Meta.BuildTag,
		Published:      false, // pending until an administrator activates it
	}

	if err := db.PutPackage(info); err != nil {
		os.Remove(pkgPath)
		return "", err
	}

	return id, nil
}

// distributePackage sends a package to all connected mesh peers. When wait > 0
// it blocks until every upload finishes or the timeout expires, which lets an
// activation finish distribution before the watchdog restarts this process.
func (s *AdminServer) distributePackage(pkgData []byte, wait time.Duration) {
	if s.peerLister == nil {
		util.LogModuleInfo("admin", "distribute: peerLister is nil")
		return
	}
	if s.meshHTTPClient == nil {
		util.LogModuleInfo("admin", "distribute: meshHTTPClient is nil")
		return
	}

	peers := s.peerLister()
	if len(peers) == 0 {
		util.LogModuleInfo("admin", "distribute: no peers")
		return
	}

	util.LogModuleInfo("admin", "distributing package to %d peers", len(peers))

	var wg sync.WaitGroup
	for _, peer := range peers {
		wg.Add(1)
		go func(nodeID string) {
			defer wg.Done()
			domain := mesh.NodeDomain(nodeID)
			url := fmt.Sprintf("https://%s/api/packages/receive", domain)
			resp, err := s.meshHTTPClient.Post(url, "application/octet-stream", io.NopCloser(io.NewSectionReader(newBytesReaderAt(pkgData), 0, int64(len(pkgData)))))
			if err != nil {
				util.LogModuleError("admin", "distribute package to %s (%s) failed: %v", nodeID, domain, err)
				return
			}
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				util.LogModuleError("admin", "distribute package to %s (%s) rejected: status=%d body=%s", nodeID, domain, resp.StatusCode, string(body))
				return
			}
			util.LogModuleInfo("admin", "distributed package to %s (%s), status=%d", nodeID, domain, resp.StatusCode)
		}(peer.NodeID)
	}

	if wait <= 0 {
		return
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		util.LogModuleInfo("admin", "package distribution complete")
	case <-time.After(wait):
		util.LogModuleWarn("admin", "package distribution still running after %v; continuing with activation", wait)
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
func pkgKey(pkg *db.PackageMeta) string {
	return pkg.Platform + "/" + pkg.Arch + "@" + pkg.Version
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
		util.LogModuleDebug("admin", "sync from %s failed: %v", nodeID, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		util.LogModuleDebug("admin", "sync from %s failed: status %d", nodeID, resp.StatusCode)
		return
	}

	var result struct {
		Packages []*db.PackageMeta `json:"packages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		util.LogModuleDebug("admin", "sync from %s decode failed: %v", nodeID, err)
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
	groups := make(map[platformArch][]*db.PackageMeta)
	for _, pkg := range result.Packages {
		if !pkg.Published {
			continue
		}
		key := platformArch{pkg.Platform, pkg.Arch}
		groups[key] = append(groups[key], pkg)
	}

	// Get local packages
	localPackages := s.listPackages()
	localKeys := make(map[string]bool) // pkgKey → have
	for _, p := range localPackages {
		localKeys[pkgKey(p)] = true
		groups[platformArch{p.Platform, p.Arch}] = append(groups[platformArch{p.Platform, p.Arch}], p)
	}

	// Per group, sort by version descending and take top N
	targetKeys := make(map[string]bool) // pkgKey → should have
	for _, pkgs := range groups {
		sort.Slice(pkgs, func(i, j int) bool {
			return p2p.CompareVersions(pkgs[i].Version, pkgs[j].Version) > 0
		})
		limit := packageRetention
		if limit > len(pkgs) {
			limit = len(pkgs)
		}
		for _, pkg := range pkgs[:limit] {
			targetKeys[pkgKey(pkg)] = true
		}
	}

	// Download packages in target set that we don't have
	downloadSuccess := true
	for _, pkg := range result.Packages {
		key := pkgKey(pkg)
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
			util.LogModuleDebug("admin", "download package %s from %s failed: %v", pkg.ID, nodeID, err)
			downloadSuccess = false
			continue
		}

		pkgData, err := io.ReadAll(dlResp.Body)
		dlResp.Body.Close()
		if err != nil {
			util.LogModuleDebug("admin", "download package %s from %s read failed: %v", pkg.ID, nodeID, err)
			downloadSuccess = false
			continue
		}

		// Verify and save
		contents, err := signing.VerifyPkgBytes(pkgData, signing.PublicKey)
		if err != nil {
			util.LogModuleDebug("admin", "download package %s from %s verify failed: %v", pkg.ID, nodeID, err)
			downloadSuccess = false
			continue
		}

		id, err := s.saveExternalPackage(pkgData, contents)
		if err != nil {
			util.LogModuleDebug("admin", "download package %s from %s save failed: %v", pkg.ID, nodeID, err)
			downloadSuccess = false
			continue
		}

		localKeys[key] = true
		util.LogModuleInfo("admin", "synced package from %s: %s (version=%s)", nodeID, id, contents.Meta.Version)

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
			key := pkgKey(pkg)
			if targetKeys[key] && !keptKeys[key] {
				keptKeys[key] = true
				continue
			}
			util.LogModuleInfo("admin", "retention: deleting %s (id=%s)", key, pkg.ID)
			os.Remove(filepath.Join(packagesDir, pkg.ID+".pkg"))
			os.Remove(filepath.Join(packagesDir, pkg.ID+".json"))
		}
		if len(localPackages) > 0 {
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
	groups := make(map[platformArch][]*db.PackageMeta)
	for _, pkg := range packages {
		key := platformArch{pkg.Platform, pkg.Arch}
		groups[key] = append(groups[key], pkg)
	}

	// Apply retention to each group
	for key, pkgs := range groups {
		s.applyRetentionForGroup(pkgs, key.platform, key.arch, "")
	}
}

// applyRetention applies retention policy to a specific platform/arch combination.
// protectVersion is never deleted (the just-activated upgrade/rollback target).
func (s *AdminServer) applyRetention(platform, arch, protectVersion string) {
	packages := s.listPackages()
	var group []*db.PackageMeta
	for _, pkg := range packages {
		if pkg.Platform == platform && pkg.Arch == arch {
			group = append(group, pkg)
		}
	}
	s.applyRetentionForGroup(group, platform, arch, protectVersion)
}

// applyRetentionForGroup applies retention to a group of packages for the same platform/arch.
// Keeps the latest packageRetention versions, but never deletes the running
// version, the pending upgrade target, the last successful version, or the
// explicitly activated version passed by the caller.
func (s *AdminServer) applyRetentionForGroup(pkgs []*db.PackageMeta, platform, arch, protectVersion string) {
	protected := make(map[string]bool, 4)
	if protectVersion != "" {
		protected[protectVersion] = true
	}
	if s.GetCurrentVersion != nil {
		if v := s.GetCurrentVersion(); v != "" {
			protected[v] = true
		}
	}
	if req := upgrade.ReadRequest(); req != nil && req.Version != "" {
		protected[req.Version] = true
	}
	if st := upgrade.ReadStatus(); st.Current != "" {
		protected[st.Current] = true
	}

	// Sort by version descending (newest first)
	sort.Slice(pkgs, func(i, j int) bool {
		return p2p.CompareVersions(pkgs[i].Version, pkgs[j].Version) > 0
	})

	if len(pkgs) <= packageRetention {
		return
	}

	// Delete packages beyond retention limit, skipping protected versions.
	for _, pkg := range pkgs[packageRetention:] {
		if protected[pkg.Version] {
			util.LogModuleInfo("admin", "retention: keeping protected %s/%s version %s (id=%s)", platform, arch, pkg.Version, pkg.ID)
			continue
		}
		util.LogModuleInfo("admin", "retention: deleting %s/%s version %s (id=%s)", platform, arch, pkg.Version, pkg.ID)
		os.Remove(filepath.Join(packagesDir, pkg.ID+".pkg"))
		os.Remove(filepath.Join(packagesDir, pkg.ID+".json"))
		_ = db.DeletePackage(pkg.ID)
	}
}

// checkForNewerVersion checks if the package version is higher than current.
// If so, exits the process so watchdog can restart with the new version.
func (s *AdminServer) checkForNewerVersion(contents *signing.PkgContents) {
	// 1. Check if platform/arch matches (using compile-time identifiers, not runtime)
	if s.GetPlatform == nil || s.GetArch == nil {
		util.LogModuleInfo("admin", "checkForNewerVersion: GetPlatform/GetArch is nil")
		return
	}
	currentPlatform := s.GetPlatform()
	currentArch := s.GetArch()
	if contents.Meta.Platform != currentPlatform || contents.Meta.Arch != currentArch {
		util.LogModuleInfo("admin", "checkForNewerVersion: platform/arch mismatch (pkg=%s/%s, current=%s/%s)",
			contents.Meta.Platform, contents.Meta.Arch, currentPlatform, currentArch)
		return
	}

	// 2. Check if version is higher
	if s.GetCurrentVersion == nil {
		util.LogModuleInfo("admin", "checkForNewerVersion: GetCurrentVersion is nil")
		return
	}
	currentVersion := s.GetCurrentVersion()
	util.LogModuleInfo("admin", "checkForNewerVersion: current version=%s", currentVersion)
	if p2p.CompareVersions(contents.Meta.Version, currentVersion) <= 0 {
		util.LogModuleInfo("admin", "checkForNewerVersion: version not higher")
		return
	}

	// Notify only. Upgrades are explicit: an administrator activates a version
	// from the admin UI, which writes the worker pin.
	util.LogModuleInfo("admin", "newer version available: %s > current %s (activate manually to apply)",
		contents.Meta.Version, currentVersion)
}
