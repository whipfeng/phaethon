package admin

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"phaethon/config"
	"phaethon/db"
)

func TestSingleConnListenerAcceptUnblocksOnClose(t *testing.T) {
	ln := newSingleConnListener(nil)
	if err := ln.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := ln.Accept(); err != io.EOF {
		t.Fatalf("Accept after Close = %v, want io.EOF", err)
	}
}

func TestServeConnReturnsAfterConnClose(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := &AdminServer{server: &http.Server{Handler: mux}}

	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		srv.ServeConn(server)
		close(done)
	}()

	// Drive one request/response over the synchronous pipe, then close.
	writeErr := make(chan error, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodGet, "http://admin/ping", nil)
		writeErr <- req.Write(client)
	}()
	br := bufio.NewReader(client)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if err := <-writeErr; err != nil {
		t.Fatalf("request write: %v", err)
	}
	client.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeConn did not return after conn close")
	}
}

func TestClientIPIgnoresSpoofedForwardingHeaders(t *testing.T) {
	// Public peers must not be able to rotate X-Forwarded-For to dodge the
	// per-source login throttle.
	r := httptest.NewRequest(http.MethodPost, "/api/login", nil)
	r.RemoteAddr = "203.0.113.9:51000"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	r.Header.Set("X-Real-Ip", "5.6.7.8")
	if got := clientIP(r); got != "203.0.113.9" {
		t.Fatalf("public peer clientIP = %q, want remote addr", got)
	}

	// Loopback proxies (nginx on the same host) are trusted.
	r = httptest.NewRequest(http.MethodPost, "/api/login", nil)
	r.RemoteAddr = "127.0.0.1:51000"
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 10.0.0.1")
	if got := clientIP(r); got != "1.2.3.4" {
		t.Fatalf("loopback peer clientIP = %q, want forwarded client", got)
	}

	// Same for a private-network proxy.
	r = httptest.NewRequest(http.MethodPost, "/api/login", nil)
	r.RemoteAddr = "192.168.1.5:51000"
	r.Header.Set("X-Real-Ip", "5.6.7.8")
	if got := clientIP(r); got != "5.6.7.8" {
		t.Fatalf("private peer clientIP = %q, want X-Real-Ip", got)
	}
}

func TestLoginBackoffEscalates(t *testing.T) {
	resetLoginSecurity()
	defer resetLoginSecurity()

	ip := "198.51.100.7"
	for i := 1; i <= captchaFailedAttempts; i++ {
		recordFailedLogin(ip)
		if captchaRequiredFor(ip) != (i >= captchaFailedAttempts) {
			t.Fatalf("after %d failures captchaRequiredFor = %v", i, captchaRequiredFor(ip))
		}
	}
	if wait := loginBlockedFor(ip); wait <= 0 {
		t.Fatalf("expected a backoff after %d failures, got %v", captchaFailedAttempts, wait)
	}

	// The next strike must wait longer than the previous one.
	first := loginBlockedFor(ip)
	recordFailedLogin(ip)
	second := loginBlockedFor(ip)
	if second <= first {
		t.Fatalf("backoff did not escalate: first=%v second=%v", first, second)
	}
	if second > loginBackoffMax+time.Second {
		t.Fatalf("backoff %v exceeds cap %v", second, loginBackoffMax)
	}

	// A successful login clears the state.
	clearFailedLogins(ip)
	if loginBlockedFor(ip) != 0 || captchaRequiredFor(ip) {
		t.Fatalf("state not cleared after success")
	}
}

func TestLoginBlockedRejectsBeforePasswordCheck(t *testing.T) {
	resetLoginSecurity()
	defer resetLoginSecurity()

	s := &AdminServer{config: &config.AdminConfig{Username: "admin", Password: "right"}}
	ip := "198.51.100.8"
	for i := 0; i < captchaFailedAttempts; i++ {
		recordFailedLogin(ip)
	}

	body := strings.NewReader(`{"username":"admin","password":"wrong"}`)
	r := httptest.NewRequest(http.MethodPost, "/api/login", body)
	r.RemoteAddr = ip + ":40000"
	rec := httptest.NewRecorder()
	s.apiLogin(rec, r)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After header")
	}
}

// TestMTLSCertRequiresVerifiedChain: a presented client certificate is only
// trusted when the TLS stack verified it against the configured client CA.
func TestMTLSCertRequiresVerifiedChain(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/secret", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	s := &AdminServer{config: &config.AdminConfig{AuthEnabled: true, Username: "admin", Token: "tok"}}
	handler := s.authMiddleware(mux)

	newReq := func(cn string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/api/secret", nil)
		r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{Subject: pkix.Name{CommonName: cn}}}}
		return r
	}

	// Unverified cert (no VerifiedChains) must not authenticate.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newReq("all"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unverified cert: status %d, want 401", rec.Code)
	}

	// Verified CN=all is accepted.
	r := newReq("all")
	r.TLS.VerifiedChains = [][]*x509.Certificate{{{Subject: pkix.Name{CommonName: "all"}}}}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("verified CN=all: status %d, want 200", rec.Code)
	}

	// Verified cert with empty CN must not match an empty nodeID.
	r = newReq("")
	r.TLS.VerifiedChains = [][]*x509.Certificate{{{}}}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("verified empty CN: status %d, want 401", rec.Code)
	}
}

func resetLoginSecurity() {
	loginAttemptMu.Lock()
	loginAttempts = make(map[string]*loginState)
	loginAttemptMu.Unlock()
	captchaStoreMu.Lock()
	captchaStore = make(map[string]captchaEntry)
	captchaStoreMu.Unlock()
}

// TestBuiltinDirectForcesAuth: the in-process path (ServeConn) must keep
// requiring authentication even when AuthEnabled is false, while the OS
// listener keeps the operator's "auth disabled" behaviour.
func TestBuiltinDirectForcesAuth(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/secret", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/packages", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	s := &AdminServer{config: &config.AdminConfig{AuthEnabled: false}}

	// OS listener path: auth disabled means everything is allowed.
	rec := httptest.NewRecorder()
	s.authMiddleware(mux).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/secret", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("OS path with AuthEnabled=false: status %d, want 200", rec.Code)
	}

	// Built-in direct path: same request must be challenged.
	direct := builtinDirectHandler{next: s.authMiddleware(mux)}
	rec = httptest.NewRecorder()
	direct.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/secret", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("built-in path with AuthEnabled=false: status %d, want 401", rec.Code)
	}

	// Public paths stay public on the built-in path (package distribution and
	// login must keep working between peers).
	rec = httptest.NewRecorder()
	direct.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/packages", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("public path via built-in path: status %d, want 200", rec.Code)
	}

	// A configured token still authenticates the built-in path via header.
	sTok := &AdminServer{config: &config.AdminConfig{AuthEnabled: false, Token: "sekret"}}
	directTok := builtinDirectHandler{next: sTok.authMiddleware(mux)}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/secret", nil)
	req.Header.Set("X-Admin-Token", "sekret")
	directTok.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("built-in path with token header: status %d, want 200", rec.Code)
	}

	// Query-string tokens are no longer accepted: they leak into logs/history.
	rec = httptest.NewRecorder()
	directTok.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/secret?token=sekret", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("query token must be rejected: status %d, want 401", rec.Code)
	}
}

// TestAPIKeyHeaderAuth covers DB-managed multi-key auth: both header forms work,
// expired keys are rejected, and a successful use records last-used time.
func TestAPIKeyHeaderAuth(t *testing.T) {
	if err := db.Init(filepath.Join(t.TempDir(), "admin-test.db")); err != nil {
		t.Fatalf("db init: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	secret := "phk_live"
	if err := db.PutAPIKey(&db.APIKey{
		ID: db.HashAPIKey(secret), Name: "live", Prefix: "phk_live", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("put key: %v", err)
	}
	expired := "phk_expired"
	exp := time.Now().Add(-time.Minute)
	if err := db.PutAPIKey(&db.APIKey{
		ID: db.HashAPIKey(expired), Name: "expired", Prefix: "phk_expire", CreatedAt: time.Now().Add(-time.Hour), ExpiresAt: &exp,
	}); err != nil {
		t.Fatalf("put expired key: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/secret", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	s := &AdminServer{config: &config.AdminConfig{AuthEnabled: true, Username: "admin"}}
	handler := s.authMiddleware(mux)

	do := func(setHeader func(*http.Request)) int {
		r := httptest.NewRequest(http.MethodGet, "/api/secret", nil)
		setHeader(r)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		return rec.Code
	}

	if code := do(func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+secret) }); code != http.StatusOK {
		t.Fatalf("Bearer live key: status %d, want 200", code)
	}
	if code := do(func(r *http.Request) { r.Header.Set("X-Admin-Token", secret) }); code != http.StatusOK {
		t.Fatalf("X-Admin-Token live key: status %d, want 200", code)
	}
	if code := do(func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+expired) }); code != http.StatusUnauthorized {
		t.Fatalf("expired key: status %d, want 401", code)
	}
	if code := do(func(r *http.Request) { r.Header.Set("Authorization", "Bearer phk_unknown") }); code != http.StatusUnauthorized {
		t.Fatalf("unknown key: status %d, want 401", code)
	}

	key, err := db.FindAPIKeyBySecret(secret)
	if err != nil {
		t.Fatalf("load key: %v", err)
	}
	if key.LastUsedAt == nil {
		t.Fatal("last_used_at was not recorded after successful auth")
	}
}

// TestAPIKeysEndpoints: create returns the plaintext once, list never contains
// it, and delete removes the record.
func TestAPIKeysEndpoints(t *testing.T) {
	if err := db.Init(filepath.Join(t.TempDir(), "api-keys-endpoints.db")); err != nil {
		t.Fatalf("db init: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	s := &AdminServer{config: &config.AdminConfig{AuthEnabled: true}}

	rec := httptest.NewRecorder()
	s.apiAPIKeys(rec, httptest.NewRequest(http.MethodPost, "/api/api-keys",
		strings.NewReader(`{"name":"ci","expires_in_days":30}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("create: status %d body %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Key    string    `json:"key"`
		Record db.APIKey `json:"record"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if !strings.HasPrefix(created.Key, "phk_") {
		t.Fatalf("unexpected key format: %q", created.Key)
	}
	if created.Record.ExpiresAt == nil {
		t.Fatal("expiry not set for expires_in_days=30")
	}

	rec = httptest.NewRecorder()
	s.apiAPIKeys(rec, httptest.NewRequest(http.MethodGet, "/api/api-keys", nil))
	if strings.Contains(rec.Body.String(), created.Key) {
		t.Fatal("list response leaked the plaintext key")
	}

	rec = httptest.NewRecorder()
	s.apiAPIKeyItem(rec, httptest.NewRequest(http.MethodDelete, "/api/api-keys/"+created.Record.ID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: status %d", rec.Code)
	}
	if _, err := db.GetAPIKey(created.Record.ID); err == nil {
		t.Fatal("key still present after delete")
	}
}

// TestEmbeddedTemplatesParse catches template syntax errors without booting
// the full admin server (parseTemplates panics on failure).
func TestEmbeddedTemplatesParse(t *testing.T) {
	s := &AdminServer{}
	s.parseTemplates()
	if s.pages == nil || s.pages.admin == nil {
		t.Fatal("admin template not parsed")
	}
}
