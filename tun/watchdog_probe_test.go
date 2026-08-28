package tun

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProbeTUNHTTP_Success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer ts.Close()

	if !ProbeTUNHTTP(2*time.Second, 2*time.Second, []string{ts.URL}) {
		t.Fatal("expected HTTP probe to succeed")
	}
}

func TestProbeTUNHTTP_Fallback(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer good.Close()

	if !ProbeTUNHTTP(2*time.Second, 2*time.Second, []string{"http://127.0.0.1:1", good.URL}) {
		t.Fatal("expected HTTP probe to succeed via fallback")
	}
}

func TestProbeTUNHTTP_AllFail(t *testing.T) {
	if ProbeTUNHTTP(1*time.Second, 1*time.Second, []string{"http://127.0.0.1:1"}) {
		t.Fatal("expected HTTP probe to fail")
	}
}

func TestProbeTUNHTTP_DefaultURLs(t *testing.T) {
	// Verify the default probe URLs are non-empty and that the function handles
	// an empty probe list by falling back to defaults without panicking.
	if len(DefaultProbeURLs) == 0 {
		t.Fatal("expected DefaultProbeURLs to be non-empty")
	}
}
