package tun

import (
	"testing"
	"time"
)

// TestHealthWatchdogDefaults verifies that the watchdog is created with the
// expected default values and that Start/Stop do not leak goroutines.
func TestHealthWatchdogDefaults(t *testing.T) {
	engine := NewEngine(nil)
	wd := NewHealthWatchdog(engine)

	if wd.interval != 5*time.Second {
		t.Errorf("interval = %v, want 5s", wd.interval)
	}
	if wd.failThreshold != 3 {
		t.Errorf("failThreshold = %d, want 3", wd.failThreshold)
	}
	if wd.probeTimeout != 3*time.Second {
		t.Errorf("probeTimeout = %v, want 3s", wd.probeTimeout)
	}

	wd.Start()
	// Stop should return promptly without deadlocking.
	done := make(chan struct{})
	go func() {
		wd.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog Stop did not return within 2s")
	}
}

// TestIsFakeIP verifies the Fake-IP range detection helper.
func TestIsFakeIP(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"198.18.0.1", true},
		{"198.19.255.255", true},
		{"198.17.0.1", false},
		{"198.20.0.1", false},
		{"8.8.8.8", false},
		{"::1", false},
	}
	for _, tc := range cases {
		if got := isFakeIP(tc.ip); got != tc.want {
			t.Errorf("isFakeIP(%q) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}
