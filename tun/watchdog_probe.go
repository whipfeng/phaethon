package tun

import (
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strings"
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
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	if len(probeURLs) == 0 {
		probeURLs = DefaultProbeURLs
	}

	client := &http.Client{
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
		probeURL := rawURL
		if strings.Contains(probeURL, "?") {
			probeURL = fmt.Sprintf("%s&tun_probe=%d", probeURL, rand.Int63())
		} else {
			probeURL = fmt.Sprintf("%s?tun_probe=%d", probeURL, rand.Int63())
		}

		resp, err := client.Get(probeURL)
		if err != nil {
			util.LogDebug("tun-watchdog: HTTP probe failed for %s: %v", rawURL, err)
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

// ProbeTUNDNS sends a DNS A query to the TUN adapter DNS proxy (192.0.2.2:53)
// from the calling process and returns true when a valid Fake-IP response is
// received. It is intended for use as an auxiliary diagnostic; the watchdog now
// prefers ProbeTUNHTTP for kill decisions because a local DNS response does not
// prove real outbound connectivity.
func ProbeTUNDNS(timeout time.Duration) bool {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}

	// Use a unique domain per probe so the system DNS cache cannot satisfy the
	// query and each probe genuinely exercises the TUN DNS proxy.
	domain := fmt.Sprintf("tun-health-%d.local", rand.Int63())
	txID := uint16(rand.Intn(65536))
	query := buildDNSQuery(domain, txID)

	addr := &net.UDPAddr{IP: net.ParseIP("192.0.2.2"), Port: 53}
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		util.LogWarn("tun-watchdog: dial DNS probe failed: %v", err)
		return false
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		util.LogWarn("tun-watchdog: set deadline failed: %v", err)
		return false
	}

	if _, err := conn.Write(query); err != nil {
		util.LogWarn("tun-watchdog: DNS probe write failed: %v", err)
		return false
	}

	resp := make([]byte, 512)
	n, err := conn.Read(resp)
	if err != nil {
		util.LogWarn("tun-watchdog: DNS probe read failed: %v", err)
		return false
	}

	ip := parseDNSResponseIP(resp[:n])
	if ip == nil {
		util.LogWarn("tun-watchdog: DNS probe response invalid")
		return false
	}

	util.LogDebug("tun-watchdog: DNS probe ok, %s -> %s", domain, ip)
	return true
}
