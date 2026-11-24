package server

import (
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func windowChangePayload(cols, rows uint32) []byte {
	return ssh.Marshal(struct {
		Cols uint32
		Rows uint32
		Wpx  uint32
		Hpx  uint32
	}{cols, rows, 0, 0})
}

// window-change requests arrive on the same stream after shell/exec; the
// handler must keep serving them for the whole session or the PTY stays at the
// size from pty-req.
func TestServeSessionRequestsAppliesWindowChange(t *testing.T) {
	reqs := make(chan *ssh.Request, 4)
	got := make(chan [2]uint32, 4)
	stop := make(chan struct{})
	go serveSessionRequests(reqs, func(cols, rows uint32) {
		got <- [2]uint32{cols, rows}
	}, stop)

	reqs <- &ssh.Request{Type: "window-change", Payload: windowChangePayload(120, 40)}
	select {
	case size := <-got:
		if size != [2]uint32{120, 40} {
			t.Fatalf("resize = %v, want [120 40]", size)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("window-change was not applied")
	}

	// 0x0 keeps the previous size, matching the pty-req handling.
	reqs <- &ssh.Request{Type: "window-change", Payload: windowChangePayload(0, 0)}
	select {
	case size := <-got:
		t.Fatalf("zero-size window-change should be ignored, got %v", size)
	case <-time.After(150 * time.Millisecond):
	}

	close(stop)
}

func TestServeSessionRequestsStopsWithSession(t *testing.T) {
	reqs := make(chan *ssh.Request)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		serveSessionRequests(reqs, func(uint32, uint32) {}, stop)
		close(done)
	}()

	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("request watcher did not stop with the session")
	}
}
