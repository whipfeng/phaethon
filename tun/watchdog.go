package tun

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"phaethon/util"
)

// HealthWatchdog monitors the TUN engine by resolving a domain through the
// system resolver, which is redirected to the TUN DNS hijacker. If the TUN
// path breaks, the resolver will fail or return a non-Fake-IP address; after
// a configurable number of consecutive failures the watchdog stops the engine
// and runs cleanup to restore network connectivity.
type HealthWatchdog struct {
	engine        *Engine
	interval      time.Duration
	failThreshold int
	probeTimeout  time.Duration
	stopCh        chan struct{}
	wg            sync.WaitGroup
}

// NewHealthWatchdog creates a watchdog for the given engine with defaults.
func NewHealthWatchdog(e *Engine) *HealthWatchdog {
	return &HealthWatchdog{
		engine:        e,
		interval:      5 * time.Second,
		failThreshold: 3,
		probeTimeout:  3 * time.Second,
		stopCh:        make(chan struct{}),
	}
}

// Start launches the watchdog goroutine.
func (w *HealthWatchdog) Start() {
	w.wg.Add(1)
	go w.run()
}

// Stop signals the watchdog goroutine to exit and waits for it.
func (w *HealthWatchdog) Stop() {
	close(w.stopCh)
	w.wg.Wait()
}

func (w *HealthWatchdog) run() {
	defer w.wg.Done()
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	failCount := 0
	for {
		select {
		case <-w.stopCh:
			return
		case <-ticker.C:
			if !w.engine.IsEnabled() {
				return
			}
			if w.probe() {
				failCount = 0
				continue
			}
			failCount++
			util.LogWarn("tun-watchdog: health check failed (%d/%d)", failCount, w.failThreshold)
			if failCount >= w.failThreshold {
				util.LogError("tun-watchdog: health check failed %d times, stopping TUN engine", failCount)
				w.engine.Stop()
				CleanupResidual()
				return
			}
		}
	}
}

// probe resolves a unique domain through the system resolver and verifies that
// the result is a Fake-IP in the 198.18.0.0/15 range.
func (w *HealthWatchdog) probe() bool {
	// Use a unique per-probe domain to avoid OS resolver caching false positives.
	domain := fmt.Sprintf("tun-health-%d.local", time.Now().UnixNano())

	ctx, cancel := context.WithTimeout(context.Background(), w.probeTimeout)
	defer cancel()

	r := &net.Resolver{}
	ips, err := r.LookupHost(ctx, domain)
	if err != nil || len(ips) == 0 {
		return false
	}

	ip := net.ParseIP(ips[0])
	if ip == nil {
		return false
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	return isFakeIP(ips[0])
}

// isFakeIP reports whether the given IP string is in the Fake-IP range
// 198.18.0.0/15.
func isFakeIP(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	return ip4[0] == 198 && ip4[1] >= 18 && ip4[1] <= 19
}
