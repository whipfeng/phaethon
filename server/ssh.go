package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"phaethon/db"
	"phaethon/util"
)

// SSHServer 是内建于每个节点的 SSH 服务。连接由 mesh 连接处理层直接移交
// （nodeID.phn:22 经 netstack 终止，不经 OS 监听端口），与 AdminServer.ServeConn
// 同一模式。仅公钥认证；shell/exec 拉起系统默认 shell。
type SSHServer struct {
	mu     sync.Mutex
	signer ssh.Signer
}

// NewSSHServer 创建 SSH 服务；host key 从 db 加载，不存在则生成 ed25519 并持久化。
func NewSSHServer() (*SSHServer, error) {
	signer, err := loadOrGenerateSSHHostKey()
	if err != nil {
		return nil, err
	}
	return &SSHServer{signer: signer}, nil
}

// HostKeyFingerprint 返回 host key 的 SHA256 指纹（供 admin 页面展示比对）。
func (s *SSHServer) HostKeyFingerprint() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.signer == nil {
		return ""
	}
	return ssh.FingerprintSHA256(s.signer.PublicKey())
}

func loadOrGenerateSSHHostKey() (ssh.Signer, error) {
	pemBytes, err := db.GetSSHHostKey()
	if err == nil && len(pemBytes) > 0 {
		if signer, err := ssh.ParsePrivateKey(pemBytes); err == nil {
			util.LogModuleInfo("ssh", "loaded ed25519 host key from database")
			return signer, nil
		} else {
			util.LogModuleWarn("ssh", "stored host key unreadable, regenerating: %v", err)
		}
	} else if err != nil && err != db.ErrNotFound {
		return nil, err
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(priv, "phaethon-host-key")
	if err != nil {
		return nil, err
	}
	encoded := pem.EncodeToMemory(block)
	if err := db.PutSSHHostKey(encoded); err != nil {
		util.LogModuleWarn("ssh", "failed to persist host key: %v", err)
	}
	util.LogModuleInfo("ssh", "generated new ed25519 host key")
	return ssh.NewSignerFromKey(priv)
}

// ServeConn 处理一条移交过来的 SSH 连接（阻塞至连接结束）。
func (s *SSHServer) ServeConn(conn net.Conn) {
	cfg, err := db.GetSSHConfig()
	if err != nil {
		util.LogModuleWarn("ssh", "read config fail, closing conn: %v", err)
		conn.Close()
		return
	}
	if !cfg.Enabled {
		util.LogModuleDebug("ssh", "ssh disabled, closing conn")
		conn.Close()
		return
	}

	s.mu.Lock()
	signer := s.signer
	s.mu.Unlock()

	serverConf := &ssh.ServerConfig{
		PublicKeyCallback: s.publicKeyCallback,
	}
	serverConf.AddHostKey(signer)

	sshConn, chans, reqs, err := ssh.NewServerConn(conn, serverConf)
	if err != nil {
		util.LogModuleDebug("ssh", "handshake fail from %s: %v", conn.RemoteAddr(), err)
		conn.Close()
		return
	}
	util.LogModuleInfo("ssh", "session opened user=%s client=%s remote=%s",
		sshConn.User(), sshConn.ClientVersion(), sshConn.RemoteAddr())
	defer sshConn.Close()

	go ssh.DiscardRequests(reqs)
	for ch := range chans {
		if ch.ChannelType() != "session" {
			ch.Reject(ssh.UnknownChannelType, "only session channels are supported")
			continue
		}
		go s.handleSession(ch)
	}
	util.LogModuleInfo("ssh", "session closed user=%s remote=%s", sshConn.User(), sshConn.RemoteAddr())
}

// publicKeyCallback 每次认证从 db 实时读取公钥列表——节点规模下 bbolt 读代价
// 可忽略，天然免除缓存一致性风险；删除公钥立即生效。
func (s *SSHServer) publicKeyCallback(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	fp := ssh.FingerprintSHA256(key)
	stored, err := db.GetSSHKey(fp)
	if err != nil || stored == nil {
		util.LogModuleInfo("ssh", "auth rejected user=%s fp=%s (unknown key)", conn.User(), fp)
		return nil, errAuthDenied
	}
	util.LogModuleInfo("ssh", "auth accepted user=%s key=%s fp=%s", conn.User(), stored.Name, fp)
	return nil, nil
}

type sshAuthError string

func (e sshAuthError) Error() string { return string(e) }

const errAuthDenied = sshAuthError("public key not authorized")

// handleSession 处理 session channel：pty-req / env 先行，shell 或 exec 触发执行。
func (s *SSHServer) handleSession(ch ssh.NewChannel) {
	channel, reqs, err := ch.Accept()
	if err != nil {
		util.LogModuleDebug("ssh", "session channel accept fail: %v", err)
		return
	}

	var (
		usePty       bool
		ptyRequested bool
		cols         uint32 = 80
		rows         uint32 = 24
		started      bool
		proc         sessionProc
	)

	defer func() {
		if proc != nil {
			proc.Kill()
		}
		channel.Close()
	}()

	for req := range reqs {
		switch req.Type {
		case "pty-req":
			ptyRequested = true
			var p struct {
				Term string
				Cols uint32
				Rows uint32
				Wpx  uint32
				Hpx  uint32
				Mode string
			}
			if err := ssh.Unmarshal(req.Payload, &p); err != nil || p.Cols == 0 || p.Rows == 0 || !ptyAvailable() {
				req.Reply(false, nil)
				continue
			}
			usePty, cols, rows = true, p.Cols, p.Rows
			req.Reply(true, nil)

		case "env":
			req.Reply(true, nil)

		case "window-change":
			var p struct {
				Cols uint32
				Rows uint32
				Wpx  uint32
				Hpx  uint32
			}
			if err := ssh.Unmarshal(req.Payload, &p); err == nil && p.Cols > 0 && p.Rows > 0 {
				cols, rows = p.Cols, p.Rows
				if proc != nil {
					proc.Resize(cols, rows)
				}
			}

		case "shell", "exec":
			if started {
				req.Reply(false, nil)
				continue
			}
			isShell := req.Type == "shell"
			cmdline := ""
			if !isShell {
				var p struct{ Command string }
				if err := ssh.Unmarshal(req.Payload, &p); err != nil || p.Command == "" {
					req.Reply(false, nil)
					continue
				}
				cmdline = p.Command
			}
			shell, userSpecified := resolveShellChoice()

			if isShell {
				// Interactive shell is PTY-only. A user-specified shell is
				// never substituted: no PTY -> explicit error. An unspecified
				// shell always has a fallback: the built-in Bubble Tea shell.
				if !usePty {
					if userSpecified {
						util.LogModuleWarn("ssh", "interactive shell %q requires a PTY but none was granted", shell)
						req.Reply(false, nil)
						channel.Stderr().Write([]byte("interactive shell requires a PTY; PTY was not granted. Refusing to start " + shell + "\n"))
						sendExitStatus(channel, 1)
						return
					}
					req.Reply(true, nil)
					runBuiltinShell(channel, cols, rows, "PTY unavailable; using the built-in shell")
					return
				}
				proc, err = startSessionProc(shell, "", true, true, cols, rows)
				if err != nil {
					if userSpecified {
						util.LogModuleWarn("ssh", "start specified shell %q failed: %v", shell, err)
						req.Reply(false, nil)
						channel.Stderr().Write([]byte("failed to start configured shell " + shell + " with a PTY: " + err.Error() + "\n"))
						sendExitStatus(channel, 1)
						return
					}
					util.LogModuleWarn("ssh", "start auto shell %q failed, using built-in: %v", shell, err)
					req.Reply(true, nil)
					runBuiltinShell(channel, cols, rows, "failed to start the system shell; using the built-in shell")
					return
				}
				started = true
				req.Reply(true, nil)
				runSession(channel, proc, true)
				return
			}

			// exec: a requested PTY must be honored. Never silently fall back
			// to pipe execution when the client explicitly asked for a TTY.
			if ptyRequested && !usePty {
				util.LogModuleWarn("ssh", "exec requested a PTY but none was granted; refusing to run")
				req.Reply(false, nil)
				channel.Stderr().Write([]byte("PTY requested but unavailable; refusing to run the command without a PTY\n"))
				sendExitStatus(channel, 1)
				return
			}
			proc, err = startSessionProc(shell, cmdline, false, usePty, cols, rows)
			if err != nil {
				util.LogModuleWarn("ssh", "start proc fail: %v", err)
				req.Reply(false, nil)
				channel.Stderr().Write([]byte("failed to start process: " + err.Error() + "\n"))
				sendExitStatus(channel, 255)
				return
			}
			started = true
			req.Reply(true, nil)
			// exec sessions must drain their output pipe even when a pty was
			// requested, otherwise the last 32KB pipe buffer is truncated.
			runSession(channel, proc, false)
			return

		default:
			req.Reply(false, nil)
		}
	}
}

// runSession 在 shell/exec 应答成功后双向搬运数据，进程结束后上报 exit-status。
//
// 退出时会卡住的两个原因（都真实存在）：
//   - Windows ConPTY：shell 进程退出后伪控制台仍然持有输出管道，Read 不会返回
//     EOF，只有 Close 才会解除阻塞；
//   - Unix pty：若后台子进程还持有 slave 端，master 的 Read 同样不会 EOF。
//
// 所以 pty/shell 会话在 Wait 返回后要给输出一个很短的 flush 窗口，然后强制关闭
// 读端，再有限等待搬运协程结束——否则客户端会在 shell 退出后一直挂着。
//
// 非 pty 的 exec（如 ssh host cat bigfile）没有这个句柄问题：进程退出后管道会
// 正常 EOF。此时必须等 io.Copy 把已读出的数据全部写完再关闭 channel，
// 否则最后一批（实测约 26KB）会被截断。
func runSession(channel ssh.Channel, proc sessionProc, needFastClose bool) {
	util.LogModuleInfo("ssh", "session start needFastClose=%v", needFastClose)
	done := make(chan struct{})
	var copied atomic.Int64
	go func() {
		_, _ = io.Copy(proc, channel)
		proc.CloseStdin()
	}()
	go func() {
		n, err := io.Copy(channel, proc)
		util.LogModuleInfo("ssh", "output copy done: bytes=%d err=%v", n, err)
		copied.Store(n)
		close(done)
	}()

	err := proc.Wait()

	if needFastClose {
		// Grace window for the last chunk (e.g. shell's logout message), then
		// force the read side closed so the output copy cannot block on a
		// lingering pty/ConPTY handle.
		select {
		case <-done:
		case <-time.After(200 * time.Millisecond):
			proc.CloseIO()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				util.LogModuleWarn("ssh", "session output did not drain after exit, closing channel")
			}
		}
	} else {
		// Plain exec: wait for the output copy to drain. The timeout only
		// guards against a child that leaks the stdout pipe after exiting.
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			proc.CloseIO()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				util.LogModuleWarn("ssh", "exec output did not drain after exit, closing channel")
			}
		}
	}

	// Drain window for plain exec: the output pipe is fully copied above, but
	// the client may still have the tail of the stream buffered when it sees
	// CHANNEL_CLOSE and exits. Sending EOF first and pausing briefly lets the
	// client consume it (observed as a ~26KB short read on 8 concurrent flows).
	if !needFastClose && copied.Load() > 256<<10 {
		_ = channel.CloseWrite()
		time.Sleep(150 * time.Millisecond)
	}

	sendExitStatus(channel, exitCodeOf(err))
}

func sendExitStatus(channel ssh.Channel, code uint32) {
	channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{code}))
}

func exitCodeOf(err error) uint32 {
	if err == nil {
		return 0
	}
	if exitErr, ok := err.(interface{ ExitCode() int }); ok {
		return uint32(exitErr.ExitCode())
	}
	return 255
}
