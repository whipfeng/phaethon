//go:build linux || darwin || windows

package main

import (
	"testing"
	"time"
)

func TestProbeInstanceLockDetectsHolder(t *testing.T) {
	dir := t.TempDir()
	if !probeInstanceLock(dir) {
		t.Fatal("fresh data dir should report the instance lock free")
	}

	release, err := acquireInstanceLock(dir)
	if err != nil {
		t.Fatalf("acquire instance lock: %v", err)
	}
	if probeInstanceLock(dir) {
		t.Fatal("probe reported the lock free while it was held")
	}

	done := make(chan bool, 1)
	go func() { done <- waitForInstanceLock(dir, 5*time.Second) }()

	// The waiter must stay blocked while the lock is held.
	time.Sleep(300 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("waitForInstanceLock returned while the lock was held")
	default:
	}

	release()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("waitForInstanceLock timed out after the lock was released")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waitForInstanceLock did not return after the lock was released")
	}

	if !probeInstanceLock(dir) {
		t.Fatal("lock should be free after release")
	}
}

func TestWaitForInstanceLockTimesOut(t *testing.T) {
	dir := t.TempDir()
	release, err := acquireInstanceLock(dir)
	if err != nil {
		t.Fatalf("acquire instance lock: %v", err)
	}
	defer release()

	if waitForInstanceLock(dir, 300*time.Millisecond) {
		t.Fatal("waitForInstanceLock should time out while the lock is held")
	}
}
