package server

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"phaethon/db"
)

// 起一条本地 TCP 连接直喂 SSHServer.ServeConn（与 netstack 移交同模式：
// 单连接、不经 OS 监听服务），验证 host key 入库、公钥认证、exec 与拒绝路径。
func pipeToServer(t *testing.T, srv *SSHServer) net.Conn {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	srvConn, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	go srv.ServeConn(srvConn)
	return conn
}

func clientConfig(t *testing.T, signer ssh.Signer) *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User:            "anyone",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}
}

func mustEd25519(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

func TestSSHServerExec(t *testing.T) {
	if err := db.Init(t.TempDir() + "/test.db"); err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	srv, err := NewSSHServer()
	if err != nil {
		t.Fatal(err)
	}
	if srv.HostKeyFingerprint() == "" {
		t.Fatal("empty host key fingerprint")
	}
	// host key 已持久化：第二个实例应复用同一把
	srv2, err := NewSSHServer()
	if err != nil {
		t.Fatal(err)
	}
	if srv2.HostKeyFingerprint() != srv.HostKeyFingerprint() {
		t.Fatal("host key not persisted")
	}

	clientKey, err := ssh.NewSignerFromKey(mustEd25519(t))
	if err != nil {
		t.Fatal(err)
	}
	fp := ssh.FingerprintSHA256(clientKey.PublicKey())
	if err := db.PutSSHKey(&db.SSHKey{
		Fingerprint: fp,
		Name:        "test",
		PubKey:      string(ssh.MarshalAuthorizedKey(clientKey.PublicKey())),
		CreatedAt:   time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// 授权密钥：exec + exit-status + 输出
	conn := pipeToServer(t, srv)
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, "pipe", clientConfig(t, clientKey))
	if err != nil {
		t.Fatal("client handshake: ", err)
	}
	client := ssh.NewClient(sshConn, chans, reqs)

	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	session.Stdout = &out
	if err := session.Run("echo hello-from-ssh"); err != nil {
		t.Fatal("exec: ", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("hello-from-ssh")) {
		t.Fatalf("unexpected output: %q", out.String())
	}
	session.Close()
	client.Close()

	// 未授权密钥应被拒
	badKey, err := ssh.NewSignerFromKey(mustEd25519(t))
	if err != nil {
		t.Fatal(err)
	}
	conn2 := pipeToServer(t, srv)
	if _, _, _, err := ssh.NewClientConn(conn2, "pipe", clientConfig(t, badKey)); err == nil {
		t.Fatal("expected auth failure for unauthorized key")
	}

	// 非 session channel 应被拒
	conn3 := pipeToServer(t, srv)
	sshConn3, _, reqs3, err := ssh.NewClientConn(conn3, "pipe", clientConfig(t, clientKey))
	if err != nil {
		t.Fatal(err)
	}
	go ssh.DiscardRequests(reqs3)
	ch, _, err := sshConn3.OpenChannel("direct-tcpip", ssh.Marshal(struct{ A string }{"x"}))
	if err == nil {
		ch.Close()
		t.Fatal("expected direct-tcpip to be rejected")
	}
	fmt.Printf("rejected as expected: %v\n", err)
	sshConn3.Close()

	// 删除公钥后立即失效
	if err := db.DeleteSSHKey(fp); err != nil {
		t.Fatal(err)
	}
	conn4 := pipeToServer(t, srv)
	if _, _, _, err := ssh.NewClientConn(conn4, "pipe", clientConfig(t, clientKey)); err == nil {
		t.Fatal("expected auth failure after key deletion")
	}
}
