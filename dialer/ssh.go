package dialer

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
	"phaethon/util"
)

// sshClientCache holds established SSH clients keyed by proxy name.
// An SSH client multiplexes many forwarded channels over one TCP connection,
// so it is reused across Dial calls for the same proxy configuration.
var (
	sshClientCache = make(map[string]*ssh.Client)
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

	if client, ok := sshClientCache[key]; ok {
		return client, nil
	}

	client, err := d.createSSHClient()
	if err != nil {
		return nil, err
	}

	sshClientCache[key] = client
	return client, nil
}

func (d *SSHDialer) removeSSHClient() {
	key := d.Proxy.Name

	sshCacheMu.Lock()
	defer sshCacheMu.Unlock()

	if client, ok := sshClientCache[key]; ok {
		client.Close()
		delete(sshClientCache, key)
	}
}

func (d *SSHDialer) createSSHClient() (*ssh.Client, error) {
	// 1. Connect to the SSH server through the proxy chain (or directly)
	nextDialer := NewDialer(d.Proxy.Next)
	conn, err := nextDialer.Dial(d.Proxy.Server, d.Proxy.Port)
	if err != nil {
		return nil, fmt.Errorf("connect to ssh server %s:%d fail: %w", d.Proxy.Server, d.Proxy.Port, err)
	}

	// 2. Build SSH client configuration
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

	if len(sshConf.Auth) == 0 {
		conn.Close()
		return nil, fmt.Errorf("no authentication method configured (need password or private-key)")
	}

	// 3. Perform SSH handshake
	addr := net.JoinHostPort(d.Proxy.Server, strconv.Itoa(d.Proxy.Port))
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, sshConf)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("ssh handshake fail: %w", err)
	}

	client := ssh.NewClient(c, chans, reqs)
	return client, nil
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
