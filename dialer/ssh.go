package dialer

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"phaethon/util"
)

const (
	// sshHandshakeTimeout limits the SSH protocol handshake (TCP connect + auth + key exchange).
	sshHandshakeTimeout = 15 * time.Second
	// sshClientIdleTimeout is the maximum time an SSH client may sit unused in the cache.
	sshClientIdleTimeout = 5 * time.Minute
)

// sshClientEntry holds a cached SSH client plus metadata for lifecycle management.
type sshClientEntry struct {
	client   *ssh.Client
	lastUsed time.Time
}

// sshClientCache holds established SSH clients keyed by proxy name.
// An SSH client multiplexes many forwarded channels over one TCP connection,
// so it is reused across Dial calls for the same proxy configuration.
var (
	sshClientCache = make(map[string]*sshClientEntry)
	sshCacheMu     sync.Mutex
)

// SSHDialer forwards TCP connections through an SSH server using
// the standard SSH port-forwarding channel (direct-tcpip).
type SSHDialer struct {
	BaseDialer
}

func (d *SSHDialer) Dial(dstAddr string, dstPort int) (net.Conn, error) {
	client, err := d.getSSHClient()
	if err != nil {
		return nil, fmt.Errorf("ssh: connect to %s:%d fail: %w", d.Proxy.Server, d.Proxy.Port, err)
	}

	addr := net.JoinHostPort(dstAddr, strconv.Itoa(dstPort))
	conn, err := client.Dial("tcp", addr)
	if err != nil {
		// Stale connection — remove from cache and retry once
		d.removeSSHClient()
		client, err = d.getSSHClient()
		if err != nil {
			return nil, fmt.Errorf("ssh: reconnect to %s:%d fail: %w", d.Proxy.Server, d.Proxy.Port, err)
		}
		conn, err = client.Dial("tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("ssh: dial %s fail: %w", addr, err)
		}
	}

	util.LogDebug("[SSH-CLI] [%s] [%s] Forwarding %s:%d via SSH %s:%d", d.Proxy.Name, d.ConnIDStr(), dstAddr, dstPort, d.Proxy.Server, d.Proxy.Port)
	return conn, nil
}

func (d *SSHDialer) getSSHClient() (*ssh.Client, error) {
	key := d.Proxy.Name

	sshCacheMu.Lock()
	defer sshCacheMu.Unlock()

	d.cleanupSSHCacheLocked()

	if e, ok := sshClientCache[key]; ok {
		e.lastUsed = time.Now()
		return e.client, nil
	}

	client, err := d.createSSHClient()
	if err != nil {
		return nil, err
	}

	sshClientCache[key] = &sshClientEntry{
		client:   client,
		lastUsed: time.Now(),
	}
	return client, nil
}

func (d *SSHDialer) removeSSHClient() {
	key := d.Proxy.Name

	sshCacheMu.Lock()
	defer sshCacheMu.Unlock()

	if e, ok := sshClientCache[key]; ok {
		e.client.Close()
		delete(sshClientCache, key)
	}
}

// cleanupSSHCacheLocked closes SSH clients that have been idle for longer than
// sshClientIdleTimeout. Caller must hold sshCacheMu.
func (d *SSHDialer) cleanupSSHCacheLocked() {
	now := time.Now()
	for k, e := range sshClientCache {
		if now.Sub(e.lastUsed) > sshClientIdleTimeout {
			e.client.Close()
			delete(sshClientCache, k)
		}
	}
}

func (d *SSHDialer) createSSHClient() (*ssh.Client, error) {
	// 1. Connect to the SSH server through the proxy chain (or directly)
	nextDialer := NewDialer(d.Proxy.Next)
	conn, err := nextDialer.Dial(d.Proxy.Server, d.Proxy.Port)
	if err != nil {
		return nil, fmt.Errorf("connect to ssh server %s:%d fail: %w", d.Proxy.Server, d.Proxy.Port, err)
	}

	// 2. Enforce a handshake timeout so a hanging server cannot stall forever.
	if err := conn.SetDeadline(time.Now().Add(sshHandshakeTimeout)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("set handshake deadline fail: %w", err)
	}

	// 3. Build SSH client configuration
	sshConf := &ssh.ClientConfig{
		User: d.Proxy.Username,
		// In a proxy context the SSH server is typically a known tunnel endpoint,
		// so we skip host-key verification to avoid managing known_hosts files.
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	if d.Proxy.PrivateKey != "" {
		keyPEM, err := d.loadPrivateKey()
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("load private key fail: %w", err)
		}
		signer, err := d.parsePrivateKey(keyPEM)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("parse private key fail: %w", err)
		}
		sshConf.Auth = append(sshConf.Auth, ssh.PublicKeys(signer))
	}

	if d.Proxy.Password != "" {
		sshConf.Auth = append(sshConf.Auth, ssh.Password(d.Proxy.Password))
	}

	// 4. Fall back to ssh-agent when no explicit credential is configured.
	var agentConn net.Conn
	if len(sshConf.Auth) == 0 {
		agentConn, err = d.trySSHAgentAuth(sshConf)
		if err != nil {
			util.LogDebug("[SSH-CLI] [%s] ssh-agent unavailable: %v", d.Proxy.Name, err)
		}
	}

	if len(sshConf.Auth) == 0 {
		conn.Close()
		if agentConn != nil {
			agentConn.Close()
		}
		return nil, fmt.Errorf("no authentication method configured (need password, private-key or ssh-agent)")
	}

	// 5. Perform SSH handshake
	addr := net.JoinHostPort(d.Proxy.Server, strconv.Itoa(d.Proxy.Port))
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, sshConf)
	if agentConn != nil {
		agentConn.Close()
	}
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("ssh handshake fail: %w", err)
	}

	// Handshake succeeded: remove deadline so the long-lived connection can idle.
	conn.SetDeadline(time.Time{})

	client := ssh.NewClient(c, chans, reqs)
	return client, nil
}

// trySSHAgentAuth attempts to add ssh-agent authentication to sshConf.
// It returns the agent connection so the caller can close it after handshake.
func (d *SSHDialer) trySSHAgentAuth(sshConf *ssh.ClientConfig) (net.Conn, error) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, fmt.Errorf("SSH_AUTH_SOCK not set")
	}
	agentConn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("connect to ssh-agent fail: %w", err)
	}
	ag := agent.NewClient(agentConn)
	sshConf.Auth = append(sshConf.Auth, ssh.PublicKeysCallback(ag.Signers))
	return agentConn, nil
}

// loadPrivateKey returns the PEM bytes for the configured private key.
// If the value starts with "-----BEGIN" it is treated as inline PEM;
// otherwise it is treated as a file path.
func (d *SSHDialer) loadPrivateKey() ([]byte, error) {
	pk := strings.TrimSpace(d.Proxy.PrivateKey)
	if strings.HasPrefix(pk, "-----") {
		return []byte(pk), nil
	}
	return os.ReadFile(pk)
}

// parsePrivateKey parses a PEM private key, automatically attempting
// passphrase decryption if private-key-passphrase or password is set.
func (d *SSHDialer) parsePrivateKey(pem []byte) (ssh.Signer, error) {
	passphrase := d.Proxy.PrivateKeyPassphrase
	if passphrase == "" {
		passphrase = d.Proxy.Password
	}

	if passphrase != "" {
		signer, err := ssh.ParsePrivateKeyWithPassphrase(pem, []byte(passphrase))
		if err == nil {
			return signer, nil
		}
		// Decryption failed — fall through to try unencrypted key
	}

	return ssh.ParsePrivateKey(pem)
}
