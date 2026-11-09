package dialer

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	// sshKeepAliveReplyTimeout × sshKeepAliveMaxMisses bounds how long a
	// keepalive reply may take before the connection is closed (~30s, like
	// the P2P heartbeat).
	sshKeepAliveReplyTimeout = 15 * time.Second
	sshKeepAliveMaxMisses    = 2
)

// sshClientEntry holds a cached SSH client plus metadata for lifecycle management.
type sshClientEntry struct {
	client   *ssh.Client
	lastUsed time.Time
	reconnMu sync.Mutex    // serialises reconnect for this key
	done     chan struct{} // signals keepalive goroutine to stop
	// refs counts open forwarded channels. The entry must never be reaped
	// while a channel is still in use (D24).
	refs atomic.Int64
}

// sshClientCache holds established SSH clients keyed by proxy name.
// An SSH client multiplexes many forwarded channels over one TCP connection,
// so it is reused across Dial calls for the same proxy configuration.
var (
	sshClientCache = make(map[string]*sshClientEntry)
	sshCacheMu     sync.Mutex
	// sshKeyLocks single-flights connection establishment per proxy without
	// holding sshCacheMu across the TCP/TLS/SSH handshake (D24).
	sshKeyLocks sync.Map // key -> *sync.Mutex
)

func sshKeyLock(key string) *sync.Mutex {
	m, _ := sshKeyLocks.LoadOrStore(key, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// sshChannelConn tracks a forwarded channel's lifetime so the cached client is
// not reaped while the channel is still open.
type sshChannelConn struct {
	net.Conn
	entry *sshClientEntry
	once  sync.Once
}

func (c *sshChannelConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.entry.refs.Add(-1) })
	return err
}

func wrapSSHConn(conn net.Conn, e *sshClientEntry) net.Conn {
	// The caller already holds one reference (acquired by getSSHClient); the
	// channel conn takes that reference over and releases it on Close.
	return &sshChannelConn{Conn: conn, entry: e}
}

// releaseSSHRef drops a reference acquired by getSSHClient. Entries with a
// positive refcount are never reaped, so the client stays usable while Dial
// is in flight.
func releaseSSHRef(e *sshClientEntry) {
	if e != nil {
		e.refs.Add(-1)
	}
}

// SSHDialer forwards TCP connections through an SSH server using
// the standard SSH port-forwarding channel (direct-tcpip).
type SSHDialer struct {
	BaseDialer
}

func (d *SSHDialer) Dial(dstAddr string, dstPort int) (net.Conn, error) {
	addr := net.JoinHostPort(dstAddr, strconv.Itoa(dstPort))

	conn, err := d.dialSSH(addr)
	if err != nil {
		return nil, err
	}

	util.LogModuleDebug("ssh", "[%s] %s forwarding %s:%d via SSH %s:%d", d.Proxy.Name, d.ConnIDStr(), dstAddr, dstPort, d.Proxy.Server, d.Proxy.Port)
	return conn, nil
}

// dialSSH dials through the cached SSH client. On failure it acquires the
// per-entry reconnect lock, checks whether another goroutine has already
// replaced the stale client, and if not, removes it and creates a fresh one.
// Concurrent callers serialise behind reconnMu so only one SSH handshake
// happens per failure episode.
func (d *SSHDialer) dialSSH(addr string) (net.Conn, error) {
	client, entry, err := d.getSSHClient()
	if err != nil {
		return nil, fmt.Errorf("ssh: connect to %s:%d fail: %w", d.Proxy.Server, d.Proxy.Port, err)
	}

	conn, err := client.Dial("tcp", addr)
	if err == nil {
		// Reference acquired by getSSHClient is transferred to the channel conn.
		return wrapSSHConn(conn, entry), nil
	}
	// Dial failed — release the reference before touching the cache.
	releaseSSHRef(entry)

	// Dial failed — the SSH connection is likely stale.
	// Acquire the per-entry lock so only one goroutine reconnects.
	entry.reconnMu.Lock()
	defer entry.reconnMu.Unlock()

	// Re-fetch: another goroutine may have already reconnected under us.
	cachedClient, cachedEntry, err := d.getSSHClient()
	if err != nil {
		return nil, fmt.Errorf("ssh: reconnect to %s:%d fail: %w", d.Proxy.Server, d.Proxy.Port, err)
	}

	// If the cached entry is the same one that just failed, no one has
	// replaced it yet — remove it and build a fresh client.
	if cachedEntry == entry {
		releaseSSHRef(cachedEntry)
		d.removeSSHClientLocked(entry)
		cachedClient, cachedEntry, err = d.getSSHClient()
		if err != nil {
			return nil, fmt.Errorf("ssh: reconnect to %s:%d fail: %w", d.Proxy.Server, d.Proxy.Port, err)
		}
	}
	// Otherwise cachedEntry is a freshly reconnected client — use it directly.

	conn, err = cachedClient.Dial("tcp", addr)
	if err != nil {
		releaseSSHRef(cachedEntry)
		return nil, fmt.Errorf("ssh: dial %s fail: %w", addr, err)
	}
	return wrapSSHConn(conn, cachedEntry), nil
}

// getSSHClient returns the cached client for this proxy with one reference
// acquired. Callers must transfer that reference to the forwarded channel
// (wrapSSHConn) or drop it with releaseSSHRef.
func (d *SSHDialer) getSSHClient() (*ssh.Client, *sshClientEntry, error) {
	key := d.Proxy.Name

	sshCacheMu.Lock()
	d.cleanupSSHCacheLocked()
	if e, ok := sshClientCache[key]; ok {
		e.lastUsed = time.Now()
		e.refs.Add(1)
		sshCacheMu.Unlock()
		return e.client, e, nil
	}
	sshCacheMu.Unlock()

	// Establish at most one connection per proxy; the handshake runs without
	// the global cache lock so unrelated SSH proxies are not serialised (D24).
	mu := sshKeyLock(key)
	mu.Lock()
	defer mu.Unlock()

	sshCacheMu.Lock()
	d.cleanupSSHCacheLocked()
	if e, ok := sshClientCache[key]; ok {
		e.lastUsed = time.Now()
		e.refs.Add(1)
		sshCacheMu.Unlock()
		return e.client, e, nil
	}
	sshCacheMu.Unlock()

	client, err := d.createSSHClient()
	if err != nil {
		return nil, nil, err
	}

	e := &sshClientEntry{
		client:   client,
		lastUsed: time.Now(),
		done:     make(chan struct{}),
	}
	e.refs.Store(1) // reference handed to the caller
	sshCacheMu.Lock()
	sshClientCache[key] = e
	sshCacheMu.Unlock()
	go d.keepAlive(e)
	return client, e, nil
}

func (d *SSHDialer) removeSSHClient() {
	key := d.Proxy.Name

	sshCacheMu.Lock()
	defer sshCacheMu.Unlock()

	if e, ok := sshClientCache[key]; ok {
		e.client.Close()
		close(e.done) // signal keepalive goroutine to stop
		delete(sshClientCache, key)
	}
}

// removeSSHClientLocked removes a specific entry from the cache.
// Caller must hold entry.reconnMu.
func (d *SSHDialer) removeSSHClientLocked(target *sshClientEntry) {
	key := d.Proxy.Name

	sshCacheMu.Lock()
	defer sshCacheMu.Unlock()

	if e, ok := sshClientCache[key]; ok && e == target {
		e.client.Close()
		close(e.done) // signal keepalive goroutine to stop
		delete(sshClientCache, key)
	}
}

// cleanupSSHCacheLocked closes SSH clients that have been idle for longer than
// sshClientIdleTimeout. Caller must hold sshCacheMu.
func (d *SSHDialer) cleanupSSHCacheLocked() {
	now := time.Now()
	for k, e := range sshClientCache {
		// Never reap a client that still has open forwarded channels (D24):
		// a long-lived connection makes no new Dial calls and would otherwise
		// be closed as "idle".
		if e.refs.Load() > 0 {
			continue
		}
		if now.Sub(e.lastUsed) > sshClientIdleTimeout {
			e.client.Close()
			close(e.done) // signal keepalive goroutine to stop
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
			util.LogModuleDebug("ssh", "[%s] ssh-agent unavailable: %v", d.Proxy.Name, err)
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

// keepAlive sends periodic global requests on the SSH connection to prevent
// firewalls or NATs from silently dropping idle connections.
// It only removes the entry if it is still the current one in the cache,
// avoiding a race where a stale keepalive kills a freshly reconnected client.
// After detecting a dead connection, it triggers a background reconnect so
// the next Dial() doesn't have to wait for the handshake.
func (d *SSHDialer) keepAlive(entry *sshClientEntry) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-entry.done:
			return // entry was removed, stop keepalive
		case <-ticker.C:
		}

		done := make(chan error, 1)
		sent := time.Now()
		go func() {
			_, _, err := entry.client.SendRequest("keepalive@golang.org", true, nil)
			done <- err
		}()
		// A busy jump host can be slow to answer; only consecutive misses
		// mean the connection is dead.
		for misses := 0; ; {
			select {
			case err := <-done:
				if err != nil {
					util.LogModuleWarn("ssh", "[%s] keepalive failed: %v", d.Proxy.Name, err)
					d.removeAndReconnect(entry)
					return
				}
				if rtt := time.Since(sent); rtt >= time.Second {
					util.LogModuleInfo("ssh", "[SSH-KA-RTT] [%s] keepalive reply after %v", d.Proxy.Name, rtt.Round(time.Millisecond))
				}
			case <-time.After(sshKeepAliveReplyTimeout):
				misses++
				if misses < sshKeepAliveMaxMisses {
					util.LogModuleWarn("ssh", "[%s] keepalive reply late (%v), waiting", d.Proxy.Name, sshKeepAliveReplyTimeout*time.Duration(misses))
					continue
				}
				util.LogModuleWarn("ssh", "[%s] keepalive timed out %d times, closing stale connection", d.Proxy.Name, misses)
				entry.client.Close()
				d.removeAndReconnect(entry)
				return
			case <-entry.done:
				return // entry was removed during keepalive wait
			}
			break
		}
	}
}

// removeAndReconnect removes a dead SSH connection from the cache and
// immediately attempts to reconnect in the background, so the next Dial()
// finds a healthy connection ready to use. Retries up to 3 times if the
// initial reconnect fails.
func (d *SSHDialer) removeAndReconnect(entry *sshClientEntry) {
	d.removeSSHClientLocked(entry)
	go func() {
		for attempt := 1; attempt <= 3; attempt++ {
			util.LogModuleDebug("ssh", "[%s] background reconnecting (attempt %d)...", d.Proxy.Name, attempt)
			_, e, err := d.getSSHClient()
			releaseSSHRef(e)
			if err != nil {
				util.LogModuleWarn("ssh", "[%s] background reconnect attempt %d failed: %v", d.Proxy.Name, attempt, err)
				if attempt < 3 {
					time.Sleep(2 * time.Second)
				}
			} else {
				util.LogModuleDebug("ssh", "[%s] background reconnect succeeded on attempt %d", d.Proxy.Name, attempt)
				return
			}
		}
		util.LogModuleError("ssh", "[%s] background reconnect failed after 3 attempts", d.Proxy.Name)
	}()
}

// PreWarm establishes the SSH connection eagerly so the first Dial does not
// pay the handshake latency.
func (d *SSHDialer) PreWarm() error {
	_, entry, err := d.getSSHClient()
	releaseSSHRef(entry)
	if err != nil {
		return err
	}
	util.LogModuleInfo("ssh", "[%s] pre-warmed SSH connection to %s:%d", d.Proxy.Name, d.Proxy.Server, d.Proxy.Port)
	return nil
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
