package tun

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"testing"
	"time"
)

func TestBindToInterface(t *testing.T) {
	if runtime.GOOS == "windows" {
		// On Windows interface-index binding uses IP_UNICAST_IF, which forces
		// packets out the specified interface but bypasses the route-table
		// next-hop. When a TUN session is active its split-tunnel routes grab
		// most traffic, so binding to the physical interface cannot reliably
		// reach external hosts. Skip this test unless explicitly enabled.
		if os.Getenv("PHAETHON_TEST_BIND") == "" {
			t.Skip("interface binding test skipped on Windows; set PHAETHON_TEST_BIND=1 to enable")
		}
	}

	// Find a working non-loopback interface to bind to.
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Skipf("cannot list interfaces: %v", err)
	}
	var idx int
	var bindIP string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil || len(addrs) == 0 {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil {
				continue
			}
			idx = iface.Index
			bindIP = ip4.String()
			break
		}
		if idx != 0 {
			break
		}
	}
	if idx == 0 || bindIP == "" {
		t.Skip("no suitable interface found")
	}

	// Start a local HTTP server on the chosen interface so the test does not
	// depend on external network reachability or the TUN routing state.
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	ln, err := net.Listen("tcp4", bindIP+":0")
	if err != nil {
		t.Fatalf("listen on %s: %v", bindIP, err)
	}
	defer ln.Close()
	server := &http.Server{Handler: mux}
	go server.Serve(ln)
	defer server.Close()

	url := fmt.Sprintf("http://%s/", ln.Addr().String())

	dialer := &net.Dialer{Timeout: 5 * time.Second, Control: bindToInterface(idx)}
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.DialContext(ctx, "tcp4", addr)
			},
		},
		Timeout: 5 * time.Second,
	}

	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("bound request failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("unexpected response: %d %q", resp.StatusCode, body)
	}
}
