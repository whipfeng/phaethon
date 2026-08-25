package dialer

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"phaethon/config"
)

const sshTestUser = "testuser"
const sshTestPassword = "testpass"

// sshTestServer wraps a minimal SSH server for testing the SSHDialer.
type sshTestServer struct {
	listener   net.Listener
	hostSigner ssh.Signer
	// firstDialFail controls whether the first direct-tcpip dial should fail.
	firstDialFail bool
	dialCount     int
}

func newSSHTestServer(t *testing.T) *sshTestServer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate host key fail: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatalf("create host signer fail: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen fail: %v", err)
	}
	srv := &sshTestServer{
		listener:   ln,
		hostSigner: signer,
	}
	go srv.run(t)
	return srv
}

func (s *sshTestServer) run(t *testing.T) {
	config := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if c.User() == sshTestUser && string(pass) == sshTestPassword {
				return nil, nil
			}
			return nil, fmt.Errorf("password rejected")
		},
		PublicKeyCallback: func(c ssh.ConnMetadata, pubKey ssh.PublicKey) (*ssh.Permissions, error) {
			if c.User() == sshTestUser {
				return nil, nil
			}
			return nil, fmt.Errorf("public key rejected")
		},
	}
	config.AddHostKey(s.hostSigner)

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			serverConn, chans, reqs, err := ssh.NewServerConn(c, config)
			if err != nil {
				c.Close()
				return
			}
			go ssh.DiscardRequests(reqs)
			go s.handleChannels(t, chans)
			_ = serverConn
		}(conn)
	}
}

func (s *sshTestServer) handleChannels(t *testing.T, chans <-chan ssh.NewChannel) {
	for newChannel := range chans {
		if newChannel.ChannelType() != "direct-tcpip" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "only direct-tcpip is supported")
			continue
		}

		var payload struct {
			DestHost string
			DestPort uint32
			SrcHost  string
			SrcPort  uint32
		}
		if err := ssh.Unmarshal(newChannel.ExtraData(), &payload); err != nil {
			_ = newChannel.Reject(ssh.Prohibited, "bad payload")
			continue
		}

		s.dialCount++
		if s.firstDialFail && s.dialCount == 1 {
			_ = newChannel.Reject(ssh.ConnectionFailed, "simulated first dial failure")
			continue
		}

		channel, requests, err := newChannel.Accept()
		if err != nil {
			continue
		}
		go ssh.DiscardRequests(requests)

		destAddr := net.JoinHostPort(payload.DestHost, strconv.Itoa(int(payload.DestPort)))
		destConn, err := net.Dial("tcp", destAddr)
		if err != nil {
			channel.Close()
			continue
		}

		go func() {
			_, _ = io.Copy(channel, destConn)
			channel.Close()
			destConn.Close()
		}()
		go func() {
			_, _ = io.Copy(destConn, channel)
			channel.Close()
			destConn.Close()
		}()
	}
}

func (s *sshTestServer) Addr() string {
	return s.listener.Addr().String()
}

func (s *sshTestServer) Close() {
	s.listener.Close()
}

func sshTestProxy(t *testing.T, srv *sshTestServer, opts ...func(*config.Proxy)) *config.Proxy {
	t.Helper()
	host, portStr, err := net.SplitHostPort(srv.Addr())
	if err != nil {
		t.Fatalf("split host port fail: %v", err)
	}
	port, _ := strconv.Atoi(portStr)
	p := &config.Proxy{
		Name:     t.Name(),
		Type:     config.ProxySSH,
		Server:   host,
		Port:     port,
		Username: sshTestUser,
		Password: sshTestPassword,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

func TestSSHDialer_PasswordAuth(t *testing.T) {
	srv := newSSHTestServer(t)
	defer srv.Close()

	// Start a tiny echo target behind the SSH server.
	target := newSSHEchoServer(t)
	defer target.Close()

	proxy := sshTestProxy(t, srv)
	dialer := &SSHDialer{BaseDialer{Proxy: proxy}}

	conn, err := dialer.Dial(target.Host(), target.Port())
	if err != nil {
		t.Fatalf("dial fail: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatalf("write fail: %v", err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read fail: %v", err)
	}
	if string(buf) != "hello" {
		t.Fatalf("unexpected response: %s", string(buf))
	}
}

func TestSSHDialer_PrivateKeyAuth(t *testing.T) {
	srv := newSSHTestServer(t)
	defer srv.Close()

	target := newSSHEchoServer(t)
	defer target.Close()

	proxy := sshTestProxy(t, srv, func(p *config.Proxy) {
		p.Password = ""
		p.PrivateKey = string(testSSHPrivateKeyPEM())
	})
	dialer := &SSHDialer{BaseDialer{Proxy: proxy}}

	conn, err := dialer.Dial(target.Host(), target.Port())
	if err != nil {
		t.Fatalf("dial fail: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("key")); err != nil {
		t.Fatalf("write fail: %v", err)
	}
	buf := make([]byte, 3)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read fail: %v", err)
	}
	if string(buf) != "key" {
		t.Fatalf("unexpected response: %s", string(buf))
	}
}

func TestSSHDialer_StaleRetry(t *testing.T) {
	srv := newSSHTestServer(t)
	defer srv.Close()
	srv.firstDialFail = true

	target := newSSHEchoServer(t)
	defer target.Close()

	proxy := sshTestProxy(t, srv)
	dialer := &SSHDialer{BaseDialer{Proxy: proxy}}

	conn, err := dialer.Dial(target.Host(), target.Port())
	if err != nil {
		t.Fatalf("dial fail: %v", err)
	}
	defer conn.Close()

	if srv.dialCount < 2 {
		t.Fatalf("expected stale retry, got dialCount=%d", srv.dialCount)
	}
}

func TestSSHDialer_NoAuthMethod(t *testing.T) {
	// Make sure ssh-agent is not used as a fallback.
	t.Setenv("SSH_AUTH_SOCK", "")

	srv := newSSHTestServer(t)
	defer srv.Close()

	target := newSSHEchoServer(t)
	defer target.Close()

	proxy := sshTestProxy(t, srv, func(p *config.Proxy) {
		p.Password = ""
	})
	dialer := &SSHDialer{BaseDialer{Proxy: proxy}}

	_, err := dialer.Dial(target.Host(), target.Port())
	if err == nil {
		t.Fatal("expected error without auth method")
	}
}

func TestSSHDialer_HandshakeTimeout(t *testing.T) {
	// Start a TCP listener that accepts but never speaks SSH.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen fail: %v", err)
	}
	defer ln.Close()

	go func() {
		c, _ := ln.Accept()
		if c != nil {
			time.Sleep(sshHandshakeTimeout + 500*time.Millisecond)
			c.Close()
		}
	}()

	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	proxy := &config.Proxy{
		Name:     "TEST_SSH_TIMEOUT",
		Type:     config.ProxySSH,
		Server:   host,
		Port:     port,
		Username: sshTestUser,
		Password: sshTestPassword,
	}
	dialer := &SSHDialer{BaseDialer{Proxy: proxy}}

	start := time.Now()
	_, err = dialer.Dial("127.0.0.1", 80)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed > sshHandshakeTimeout+2*time.Second {
		t.Fatalf("timeout too long: %v", elapsed)
	}
}

// sshEchoServer is a simple TCP echo server used as the target of direct-tcpip.
type sshEchoServer struct {
	listener net.Listener
}

func newSSHEchoServer(t *testing.T) *sshEchoServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen fail: %v", err)
	}
	s := &sshEchoServer{listener: ln}
	go s.run()
	return s
}

func (s *sshEchoServer) run() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			buf := make([]byte, 1024)
			for {
				n, err := c.Read(buf)
				if err != nil {
					return
				}
				if _, err := c.Write(buf[:n]); err != nil {
					return
				}
			}
		}(conn)
	}
}

func (s *sshEchoServer) Host() string {
	host, _, _ := net.SplitHostPort(s.listener.Addr().String())
	return host
}

func (s *sshEchoServer) Port() int {
	_, portStr, _ := net.SplitHostPort(s.listener.Addr().String())
	port, _ := strconv.Atoi(portStr)
	return port
}

func (s *sshEchoServer) Close() {
	s.listener.Close()
}

func testSSHPrivateKeyPEM() []byte {
	// Generated on demand for tests.
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	block, _ := ssh.MarshalPrivateKey(key, "")
	return pem.EncodeToMemory(block)
}
