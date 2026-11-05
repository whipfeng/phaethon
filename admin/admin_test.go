package admin

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
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
