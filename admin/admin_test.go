package admin

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"phaethon/config"
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

	// A configured token still authenticates the built-in path.
	sTok := &AdminServer{config: &config.AdminConfig{AuthEnabled: false, Token: "sekret"}}
	directTok := builtinDirectHandler{next: sTok.authMiddleware(mux)}
	rec = httptest.NewRecorder()
	directTok.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/secret?token=sekret", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("built-in path with token: status %d, want 200", rec.Code)
	}
}
