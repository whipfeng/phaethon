package tun

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"time"

	"phaethon/util"
)

// DefaultProbeURLs is the default list of HTTP endpoints used by the TUN
// watchdog to verify real outbound connectivity. They are public captive-portal
// style URLs chosen because they are lightweight, fast, and widely reachable.
var DefaultProbeURLs = []string{
	"http://cp.cloudflare.com/generate_204",           // Cloudflare, very fast
	"http://detectportal.firefox.com/success.txt",     // Firefox
	"http://www.msftconnecttest.com/connecttest.txt",  // Microsoft
}

// ProbeTUNHTTP sends HTTP GET requests to the given URLs and returns true if
// any of them succeeds. A unique query parameter is appended to each request to
// bypass caches. The request goes through the system network stack, so when TUN
// is active it exercises the full TUN → netstack → proxy chain.
func ProbeTUNHTTP(dnsTimeout, httpTimeout time.Duration, probeURLs []string) bool {
	return ProbeTUNHTTPWithBind(dnsTimeout, httpTimeout, 0, probeURLs)
}

// ProbeTUNHTTPWithBind is like ProbeTUNHTTP but forces the HTTP client to bind
// its outgoing TCP sockets to the given TUN network interface index. This
// ensures probe traffic egresses through the TUN adapter and cannot be silently
// routed around TUN by source-address selection or stale DNS state. An ifIndex
// <= 0 means no binding.
//
// DNS resolution uses a pure Go resolver (PreferGo: true) to avoid OS thread
// blocking on Windows. The resolver sends UDP queries to the system-configured
// DNS server, which in TUN mode is the TUN DNS hijacker.
func ProbeTUNHTTPWithBind(dnsTimeout, httpTimeout time.Duration, ifIndex int, probeURLs []string) bool {
	if dnsTimeout <= 0 {
		dnsTimeout = 5 * time.Second
	}
	if httpTimeout <= 0 {
		httpTimeout = 8 * time.Second
	}
	if len(probeURLs) == 0 {
		probeURLs = DefaultProbeURLs
	}

	// Pure Go DNS resolver to avoid OS thread blocking on Windows.
	// The system DNS is configured to point to the TUN DNS hijacker (192.0.2.2),
	// so queries will be intercepted by the hijacker.
	resolver := &net.Resolver{
		PreferGo:     true,
		StrictErrors: false,
	}

	// Bind to the TUN adapter using IP_UNICAST_IF (Windows) or
	// SO_BINDTODEVICE/IP_BOUND_IF (Linux/Darwin) to force probe traffic
	// through TUN.
	dialer := &net.Dialer{Timeout: httpTimeout}
	if ifIndex > 0 {
		if ctrl := watchdogControl(ifIndex); ctrl != nil {
			dialer.Control = ctrl
		}
	}

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				// Force IPv4 so interface binding (bind to local IP) is
				// unambiguous and does not depend on dual-stack socket behavior.
				return dialer.DialContext(ctx, "tcp4", addr)
			},
		},
		Timeout: httpTimeout,
		// Avoid following redirects; we only care about reachability.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	for _, rawURL := range probeURLs {
		if rawURL == "" {
			continue
		}

		u, err := url.Parse(rawURL)
		if err != nil {
			util.LogWarn("tun-watchdog: invalid probe URL %q: %v", rawURL, err)
			continue
		}

		// Resolve the hostname using the pure Go DNS resolver. The query goes to
		// the system-configured DNS server (TUN DNS hijacker at 192.0.2.2), which
		// returns a Fake-IP. The engine will handle the Fake-IP connection.
		resolveCtx, cancel := context.WithTimeout(context.Background(), dnsTimeout)
		ips, err := resolver.LookupIP(resolveCtx, "ip4", u.Hostname())
		cancel()
		if err != nil || len(ips) == 0 {
			util.LogWarn("tun-watchdog: IPv4 resolution failed for %s: %v", u.Hostname(), err)
			continue
		}
		ip := ips[0].To4()
		if ip == nil {
			util.LogWarn("tun-watchdog: no IPv4 address for %s", u.Hostname())
			continue
		}

		// Preserve the original Host header for virtual hosting; replace the
		// URL host with the resolved Fake-IP so the dialer does not do DNS.
		originalHost := u.Host
		port := u.Port()
		if port == "" {
			port = "80"
		}
		u.Host = net.JoinHostPort(ip.String(), port)

		q := u.Query()
		q.Set("tun_probe", fmt.Sprintf("%d", rand.Int63()))
		u.RawQuery = q.Encode()

		req, err := http.NewRequest(http.MethodGet, u.String(), nil)
		if err != nil {
			util.LogWarn("tun-watchdog: failed to build request for %s: %v", rawURL, err)
			continue
		}
		req.Host = originalHost

		resp, err := client.Do(req)
		if err != nil {
			util.LogWarn("tun-watchdog: HTTP probe failed for %s: %v", rawURL, err)
			continue
		}

		// Drain and discard the body so the connection can be reused or closed cleanly.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		// Treat any 2xx or 3xx as success: some captive portals redirect and
		// that still proves the path is up.
		if resp.StatusCode >= 200 && resp.StatusCode < 400 {
			util.LogDebug("tun-watchdog: HTTP probe ok, %s -> %d", rawURL, resp.StatusCode)
			return true
		}

		util.LogDebug("tun-watchdog: HTTP probe unexpected status for %s: %d", rawURL, resp.StatusCode)
	}

	util.LogWarn("tun-watchdog: HTTP probe failed for all %d URLs", len(probeURLs))
	return false
}

