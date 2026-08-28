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
// style URLs chosen because they are lightweight and widely reachable.
var DefaultProbeURLs = []string{
	"http://www.msftconnecttest.com/connecttest.txt",
	"http://connectivitycheck.platform.hicloud.com/generate_204",
	"http://wifi.vivo.com.cn/generate_204",
}

// ProbeTUNHTTP sends HTTP GET requests to the given URLs and returns true if
// any of them succeeds. A unique query parameter is appended to each request to
// bypass caches. The request goes through the system network stack, so when TUN
// is active it exercises the full TUN → netstack → proxy chain.
func ProbeTUNHTTP(timeout time.Duration, probeURLs []string) bool {
	return ProbeTUNHTTPWithBind(timeout, 0, probeURLs)
}

// ProbeTUNHTTPWithBind is like ProbeTUNHTTP but forces the HTTP client to bind
// its outgoing TCP sockets to the given TUN network interface index. This
// ensures probe traffic egresses through the TUN adapter and cannot be silently
// routed around TUN by source-address selection or stale DNS state. An ifIndex
// <= 0 means no binding.
//
// DNS resolution uses the system resolver, which goes through the TUN DNS
// hijacker. The hijacker synchronously resolves the real IP and caches it in
// the FakeIPPool, so the engine can dial the real IP directly without
// re-resolving the domain.
func ProbeTUNHTTPWithBind(timeout time.Duration, ifIndex int, probeURLs []string) bool {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	if len(probeURLs) == 0 {
		probeURLs = DefaultProbeURLs
	}

	// Bind to the TUN adapter's local IP to force probe traffic through TUN.
	// This uses bind() to the interface's source address, which works for all
	// adapter types including Wintun (unlike IP_UNICAST_IF which fails on L3
	// virtual adapters).
	dialer := &net.Dialer{Timeout: timeout}
	if ifIndex > 0 {
		dialer.Control = watchdogControl(ifIndex)
	}

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				// Force IPv4 so interface binding (bind to local IP) is
				// unambiguous and does not depend on dual-stack socket behavior.
				return dialer.DialContext(ctx, "tcp4", addr)
			},
		},
		Timeout: timeout,
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

		// Resolve the hostname through the system DNS, which goes through the
		// TUN DNS hijacker. The hijacker synchronously resolves the real IP and
		// caches it, so the engine can dial directly without re-resolution.
		resolveCtx, cancel := context.WithTimeout(context.Background(), timeout)
		ips, err := net.DefaultResolver.LookupIP(resolveCtx, "ip4", u.Hostname())
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

