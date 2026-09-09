package dialer

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"phaethon/config"
	"phaethon/util"
)

// mockHTunnelServer creates a minimal HTTP tunnel server for testing.
func mockHTunnelServer(t *testing.T) *httptest.Server {
	seq := 0
	connectionID := "test-conn-123"
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Logf("mock server: %s %s", r.Method, r.URL.Path)
		switch r.Method {
		case "HEAD":
			if seq == 0 {
				w.Header().Set(headerConnectionID, connectionID)
			}
			w.WriteHeader(200)
			seq++
		case "GET":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(200)
			w.Write([]byte("hello"))
		case "POST":
			body, _ := io.ReadAll(r.Body)
			t.Logf("POST received: %d bytes", len(body))
			w.WriteHeader(200)
		case "PUT":
			w.WriteHeader(200)
		case "DELETE":
			w.WriteHeader(410)
		}
	}))
}

// TestHTunnelDialer_Dial_ConnectTimeout verifies the dialer fails fast
// when the server is unreachable.
func TestHTunnelDialer_Dial_ConnectTimeout(t *testing.T) {
	d := HTunnelDialer{
		BaseDialer: BaseDialer{
			Proxy: &config.Proxy{
				URL:      "http://127.0.0.1:1/", // nothing listening
				Password: "test",
			},
		},
	}

	_, err := d.Dial("10.0.0.1", 80)
	if err == nil {
		t.Fatal("expected error for unreachable server")
	}
	// Should fail within connect timeout (10s), not hang forever
	start := time.Now()
	for i := 0; i < 3; i++ {
		d.Dial("10.0.0.1", 80)
	}
	if time.Since(start) > 15*time.Second {
		t.Error("dial took too long, timeout not effective")
	}
}

// TestHTunnelDialer_DialUsesConn verifies Dial() always uses CONN command.
func TestHTunnelDialer_DialUsesConn(t *testing.T) {
	var capturedCmd string

	headCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			if headCount == 0 {
				capturedCmd = r.Header.Get(headerCommand)
				w.Header().Set(headerConnectionID, "test-id")
			}
			w.WriteHeader(200)
			headCount++
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	d := HTunnelDialer{
		BaseDialer: BaseDialer{
			Proxy: &config.Proxy{
				URL:      srv.URL + "/",
				Password: "testpass",
			},
		},
	}

	conn, err := d.Dial("example.com", 8080)
	if err != nil {
		t.Fatalf("dial error: %v", err)
	}
	defer conn.Close()

	if capturedCmd != "CONN" {
		t.Errorf("expected CONN command, got: %s", capturedCmd)
	}
}

// TestHTunnelDialer_ReverseNotInRegistry verifies TryReverse returns error
// when registry is not initialized.
func TestHTunnelDialer_ReverseNotInRegistry(t *testing.T) {
	d := HTunnelDialer{
		BaseDialer: BaseDialer{
			Proxy: &config.Proxy{
				ReverseAddress: "test-addr",
			},
		},
	}

	_, err := d.Dial("", 0)
	if err == nil {
		t.Fatal("expected error when registry not initialized")
	}
}

// TestHTunnelConn_ReadWrite_AEAD verifies payload AEAD encryption/decryption.
func TestHTunnelConn_ReadWrite_AEAD(t *testing.T) {
	plaintext := []byte("hello world")
	password := "aead-test"

	crypto := util.NewHTunnelCrypto(password)

	// Simulate write path: seal
	sealed := crypto.SealBody(plaintext)

	// Verify sealing changed the data (nonce + tag)
	if len(sealed) <= len(plaintext) {
		t.Fatal("AEAD sealing did not expand data")
	}

	// Simulate read path: open
	decrypted, err := crypto.OpenBody(sealed)
	if err != nil {
		t.Fatalf("AEAD open failed: %v", err)
	}

	if !bytes.Equal(decrypted, plaintext) {
		t.Errorf("AEAD decrypt mismatch: %q vs %q", decrypted, plaintext)
	}
}

// TestHTunnelConn_RequestTimeout verifies short requests (POST/DELETE) use 10s timeout.
func TestHTunnelConn_RequestTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(15 * time.Second) // longer than 10s timeout
		w.WriteHeader(200)
	}))
	defer srv.Close()

	conn := &htunnelConn{
		proxy:        &config.Proxy{URL: srv.URL + "/", Password: "test"},
		connectionID: "test",
		client: &http.Client{
			Transport: &http.Transport{
				DialContext: (&net.Dialer{Timeout: 1 * time.Second}).DialContext,
			},
		},
		closed: make(chan struct{}),
		crypto: util.NewHTunnelCrypto("test"),
	}

	start := time.Now()
	_, err := conn.Write([]byte("test"))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed > 12*time.Second {
		t.Errorf("timeout too slow: %v", elapsed)
	}
	if elapsed < 5*time.Second {
		t.Errorf("timeout too fast: %v", elapsed)
	}
}

// TestHTunnelConn_ReadTimeout verifies GET uses 40s timeout.
func TestHTunnelConn_ReadTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			time.Sleep(45 * time.Second) // longer than 40s timeout
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	conn := &htunnelConn{
		proxy:        &config.Proxy{URL: srv.URL + "/", Password: "test"},
		connectionID: "test",
		client: &http.Client{
			Transport: &http.Transport{
				DialContext: (&net.Dialer{Timeout: 1 * time.Second}).DialContext,
			},
		},
		closed: make(chan struct{}),
		crypto: util.NewHTunnelCrypto("test"),
	}

	start := time.Now()
	buf := make([]byte, 100)
	_, err := conn.Read(buf)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed > 42*time.Second {
		t.Errorf("read timeout too slow: %v", elapsed)
	}
	if elapsed < 35*time.Second {
		t.Errorf("read timeout too fast: %v", elapsed)
	}
}
