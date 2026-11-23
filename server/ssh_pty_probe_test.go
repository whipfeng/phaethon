package server

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// ansiSeq matches CSI sequences (colors, cursor control) and OSC titles, so a
// probe line can be compared without terminal decoration.
var ansiSeq = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]|\x1b\][^\x07]*\x07`)

// hasProbeResult reports whether the shell actually *executed* the probe
// command: a line equal to the sentinel, after stripping ANSI codes. Matching
// the echoed command line ("echo PHAE_PROBE_OK") is not enough.
func hasProbeResult(s string) bool {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(ansiSeq.ReplaceAllString(line, ""), "\r")
		if strings.TrimSpace(line) == "PHAE_PROBE_OK" {
			return true
		}
	}
	return false
}

// TestPtyProbeShell is a manual probe: it starts the given shell through the
// exact same PTY path the built-in SSH server uses and checks whether a
// command round-trips.
//
//	go test ./server -run TestPtyProbeShell -v -timeout 60s
//
// Skipped unless PHAE_PTY_PROBE is set (format: "<shell path>").
func TestPtyProbeShell(t *testing.T) {
	spec := os.Getenv("PHAE_PTY_PROBE")
	if spec == "" {
		t.Skip("set PHAE_PTY_PROBE=<shell path> to run")
	}
	shell := spec
	if i := strings.Index(spec, "|"); i >= 0 {
		shell = spec[:i]
	}

	if !ptyAvailable() {
		t.Fatalf("PTY unavailable on this host")
	}
	p, err := startSessionProc(shell, "", true, true, 120, 40)
	if err != nil {
		t.Fatalf("startSessionProc(%q): %v", shell, err)
	}
	defer func() { p.Kill(); p.CloseIO() }()

	out := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := p.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
				if hasProbeResult(sb.String()) {
					out <- sb.String()
					return
				}
			}
			if err != nil {
				out <- sb.String()
				return
			}
		}
	}()

	time.Sleep(1500 * time.Millisecond)
	if _, err := p.Write([]byte("echo PHAE_PROBE_OK\r")); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case s := <-out:
		t.Logf("output:\n%s", s)
		if !hasProbeResult(s) {
			t.Fatalf("shell did not execute the probe command")
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("timed out waiting for shell output (shell likely not interactive)")
	}
}
