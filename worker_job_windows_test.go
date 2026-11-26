//go:build windows

package main

import (
	"os/exec"
	"testing"
	"time"
)

// TestWorkerJobKillsProcessOnClose verifies the kernel-level guarantee the
// handoff fix depends on: closing the watchdog's job terminates its workers, so
// an orphaned worker can never keep holding the instance lock.
func TestWorkerJobKillsProcessOnClose(t *testing.T) {
	job := createWorkerJob()
	if job == nil {
		t.Fatal("createWorkerJob returned nil")
	}

	cmd := exec.Command("ping", "-n", "60", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	pid := cmd.Process.Pid
	if err := job.assign(pid); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("assign child to job: %v", err)
	}

	job.close()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processExists(pid) {
			_ = cmd.Wait()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	t.Fatal("child survived job close; KILL_ON_JOB_CLOSE not effective")
}
