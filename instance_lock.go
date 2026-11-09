package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// instanceConflictExitCode is the exit code a worker uses when it cannot take
// the single-instance lock because another instance already owns the data
// directory. The watchdog treats this code as "do not restart" and exits, so
// two instances never fight over the TUN device, routes or the database.
const instanceConflictExitCode = 3

// instanceLockHolder is the lock file content. The OS lock is authoritative;
// this record only exists to diagnose conflicts and to let --takeover verify
// that the process it is about to terminate is really this deployment.
type instanceLockHolder struct {
	PID     int    `json:"pid"`
	Exe     string `json:"exe"`
	Started string `json:"started"`
}

func (h instanceLockHolder) describe() string {
	if h.PID <= 0 {
		return "unknown holder"
	}
	return fmt.Sprintf("pid=%d exe=%s since=%s", h.PID, h.Exe, h.Started)
}

// writeLockHolder records who owns the lock, for diagnostics only.
func writeLockHolder(f *os.File) {
	exe, _ := os.Executable()
	holder := instanceLockHolder{
		PID:     os.Getpid(),
		Exe:     exe,
		Started: time.Now().Format(time.RFC3339),
	}
	data, err := json.Marshal(holder)
	if err != nil {
		return
	}
	_ = f.Truncate(0)
	_, _ = f.WriteAt(append(data, '\n'), 0)
}

// readLockHolder parses the recorded owner. ok is false for empty, truncated
// or legacy-format files.
func readLockHolder(f *os.File) (instanceLockHolder, bool) {
	buf := make([]byte, 1024)
	n, _ := f.ReadAt(buf, 0)
	if n <= 0 {
		return instanceLockHolder{}, false
	}
	var h instanceLockHolder
	if err := json.Unmarshal(buf[:n], &h); err != nil {
		return instanceLockHolder{}, false
	}
	return h, h.PID > 0
}
