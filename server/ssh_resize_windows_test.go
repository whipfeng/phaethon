//go:build windows

package server

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"phaethon/db"
)

// Regression test for interactive sessions that never followed terminal
// resizes: the request loop was abandoned as soon as the shell/exec started,
// so later window-change requests were dropped and the ConPTY kept the size
// from pty-req. The shell reports the console width before and after a resize.
func TestSSHWindowChangeResizesConPTY(t *testing.T) {
	if !supportConPTY {
		t.Skip("ConPTY not available")
	}
	powershell, err := exec.LookPath("powershell")
	if err != nil {
		t.Skip("powershell not available")
	}
	if err := db.Init(t.TempDir() + "/test.db"); err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.PutSSHConfig(&db.SSHConfig{Enabled: true, Shell: powershell}); err != nil {
		t.Fatal(err)
	}

	srv, err := NewSSHServer()
	if err != nil {
		t.Fatal(err)
	}
	clientKey, err := ssh.NewSignerFromKey(mustEd25519(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutSSHKey(&db.SSHKey{
		Fingerprint: ssh.FingerprintSHA256(clientKey.PublicKey()),
		Name:        "resize-test",
		PubKey:      string(ssh.MarshalAuthorizedKey(clientKey.PublicKey())),
		CreatedAt:   time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	conn := pipeToServer(t, srv)
	defer conn.Close()
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, "pipe", clientConfig(t, clientKey))
	if err != nil {
		t.Fatal("client handshake: ", err)
	}
	client := ssh.NewClient(sshConn, chans, reqs)
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err != nil {
		t.Fatal("pty-req: ", err)
	}

	widthPath := filepath.Join(t.TempDir(), "width.txt")
	cmdline := fmt.Sprintf(
		"[Console]::WindowWidth | Set-Content -Path '%s'; Start-Sleep -Seconds 3; [Console]::WindowWidth | Set-Content -Path '%s'",
		widthPath, widthPath)
	if err := session.Start(cmdline); err != nil {
		t.Fatal("start: ", err)
	}

	waitConsoleWidth(t, widthPath, "80", 15*time.Second)
	if err := session.WindowChange(40, 120); err != nil {
		t.Fatal("window-change: ", err)
	}
	waitConsoleWidth(t, widthPath, "120", 15*time.Second)
	_ = session.Wait()
}

func waitConsoleWidth(t *testing.T, path, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			last = strings.TrimSpace(string(b))
			if last == want {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("console width never became %s (last=%q)", want, last)
}
