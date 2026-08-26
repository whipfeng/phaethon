package tun

import (
	"fmt"
	"math/rand"
	"net"
	"time"

	"phaethon/util"
)

// ProbeTUNDNS sends a DNS A query to the TUN adapter DNS proxy (192.0.2.2:53)
// from the calling process and returns true when a valid Fake-IP response is
// received. It is intended for use by the external watchdog process, which runs
// outside the phaethon service and can therefore exercise the real system
// resolver/TUN path.
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
