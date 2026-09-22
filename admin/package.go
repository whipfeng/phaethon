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
	"time"

	"phaethon/pkg/signing"
	"phaethon/util"
)

const (
	// packagesDir is where uploaded .pkg files are stored.
	packagesDir = ".phaethon/packages"

	defaultPackageUploadMaxMB = 100
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
	ID             string            `json:"id"`
	Filename       string            `json:"filename"`
	Size           int64             `json:"size"`
	UploadedAt     time.Time         `json:"uploadedAt"`
	SignatureValid bool              `json:"signatureValid"`
	Meta           signing.PackageMeta `json:"meta"`
	Published      bool              `json:"published"`
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

	// Mark this as published, others as unpublished
	packages := s.listPackages()
	for _, pkg := range packages {
		if pkg.ID == id {
			pkg.Published = true
		} else {
			pkg.Published = false
		}
		if err := s.savePackageMeta(pkg); err != nil {
			httpError(w, "update metadata: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	util.LogInfo("[ADMIN] package published: %s (version=%s)", id, info.Meta.Version)
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
