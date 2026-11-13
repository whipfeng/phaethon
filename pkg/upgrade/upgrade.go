// Package upgrade defines the on-disk contract between the admin API (which
// requests an upgrade) and the watchdog (which performs the self-replacement).
//
// The files are deliberately plain and relative to the deployment working
// directory:
//
//	data/state/upgrade-request     {version,...}  written by activate
//	data/state/upgrade-status      {status,...}   written by the watchdog
//	data/state/upgrade-in-progress {old,new,...}  crash-recovery marker
//	data/state/worker-version      last ready version reported by the worker
package upgrade

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Status values reported by the watchdog.
const (
	StatusIdle             = "idle"
	StatusApplying         = "applying"
	StatusSucceeded        = "succeeded"
	StatusFailedRolledBack = "failed-rolled-back"
)

// Paths are relative to the process working directory, matching the rest of
// the data/ layout.
const (
	RequestPath = "data/state/upgrade-request"
	StatusPath  = "data/state/upgrade-status"
	MarkerPath  = "data/state/upgrade-in-progress"
	VersionPath = "data/state/worker-version"
)

// Request is written by an explicit admin activation. The watchdog polls it.
// PackageID is best-effort: received packages get a new local random ID, so
// the watchdog matches by version + platform/arch instead of relying on it.
type Request struct {
	Version     string    `json:"version"`
	PackageID   string    `json:"packageId,omitempty"`
	RequestedAt time.Time `json:"requestedAt"`
}

// Status is the watchdog's report of the last upgrade attempt.
type Status struct {
	Status    string    `json:"status"`
	Current   string    `json:"current,omitempty"`
	Requested string    `json:"requested,omitempty"`
	LastError string    `json:"lastError,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Marker records an in-flight swap so a crash/power loss can be repaired at
// the next watchdog start.
type Marker struct {
	Old       string    `json:"old"`
	New       string    `json:"new"`
	Version   string    `json:"version"`
	StartedAt time.Time `json:"startedAt"`
}

// WriteRequest atomically replaces the pending upgrade request.
func WriteRequest(req Request) error {
	if req.RequestedAt.IsZero() {
		req.RequestedAt = time.Now()
	}
	return writeJSON(RequestPath, req)
}

// ReadRequest returns nil when there is no request file.
func ReadRequest() *Request {
	var req Request
	if !readJSON(RequestPath, &req) || req.Version == "" {
		return nil
	}
	return &req
}

// ClearRequest removes the pending request.
func ClearRequest() error {
	return removeIfExists(RequestPath)
}

// WriteStatus atomically replaces the status file.
func WriteStatus(st Status) error {
	if st.Status == "" {
		st.Status = StatusIdle
	}
	st.UpdatedAt = time.Now()
	return writeJSON(StatusPath, st)
}

// ReadStatus returns an idle status when the file is missing or invalid.
func ReadStatus() Status {
	var st Status
	if !readJSON(StatusPath, &st) || st.Status == "" {
		return Status{Status: StatusIdle}
	}
	return st
}

// WriteMarker persists the crash-recovery marker before the swap begins.
func WriteMarker(m Marker) error {
	if m.StartedAt.IsZero() {
		m.StartedAt = time.Now()
	}
	return writeJSON(MarkerPath, m)
}

// ReadMarker returns nil when no swap was in flight.
func ReadMarker() *Marker {
	var m Marker
	if !readJSON(MarkerPath, &m) || (m.Old == "" && m.New == "") {
		return nil
	}
	return &m
}

// ClearMarker removes the crash-recovery marker.
func ClearMarker() error {
	return removeIfExists(MarkerPath)
}

// WriteWorkerVersion persists the version reported by the running worker.
func WriteWorkerVersion(version string) error {
	if version == "" {
		return nil
	}
	return writeFileAtomic(VersionPath, []byte(version))
}

// ReadWorkerVersion returns the last persisted ready version, or "".
func ReadWorkerVersion() string {
	data, err := os.ReadFile(VersionPath)
	if err != nil {
		return ""
	}
	return string(trimSpace(data))
}

func writeJSON(path string, v interface{}) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data)
}

func readJSON(path string, v interface{}) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return json.Unmarshal(data, v) == nil
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	// The request/marker/status must be durable before the swap starts;
	// without fsync a power loss can leave a rename-visible but empty file.
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// Windows can transiently fail the rename while the admin polls the status
	// file. Retry briefly instead of silently losing a state transition.
	var renameErr error
	for attempt := 0; attempt < 5; attempt++ {
		if err := os.Rename(tmp, path); err == nil {
			renameErr = nil
			break
		} else {
			renameErr = err
			time.Sleep(50 * time.Millisecond)
		}
	}
	if renameErr != nil {
		_ = os.Remove(tmp)
		return renameErr
	}
	syncDir(filepath.Dir(path))
	return nil
}

// syncDir flushes the directory entry on platforms that support it. Failures
// are ignored: the file contents were already fsynced.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

func removeIfExists(path string) error {
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func trimSpace(b []byte) string {
	start, end := 0, len(b)
	for start < end && (b[start] == ' ' || b[start] == '\n' || b[start] == '\r' || b[start] == '\t') {
		start++
	}
	for end > start && (b[end-1] == ' ' || b[end-1] == '\n' || b[end-1] == '\r' || b[end-1] == '\t') {
		end--
	}
	return string(b[start:end])
}
