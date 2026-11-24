//go:build !windows

package server

import (
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"
)

type ptyProc struct {
	cmd *exec.Cmd
	f   *os.File

	// mu guards closed. A late window-change may race the session teardown;
	// resizing an already closed fd could hit an unrelated file that reused
	// the descriptor number.
	mu     sync.Mutex
	closed bool
}

func (p *ptyProc) Read(b []byte) (int, error)  { return p.f.Read(b) }
func (p *ptyProc) Write(b []byte) (int, error) { return p.f.Write(b) }
func (p *ptyProc) Wait() error                 { return p.cmd.Wait() }
func (p *ptyProc) Kill() {
	_ = p.cmd.Process.Kill()
	p.closeLocked()
}
func (p *ptyProc) CloseStdin() {}
func (p *ptyProc) CloseIO()    { p.closeLocked() }

// closeLocked closes the master fd at most once. Callers must hold p.mu.
func (p *ptyProc) closeLocked() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	_ = p.f.Close()
}

func (p *ptyProc) Resize(cols, rows uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	_ = pty.Setsize(p.f, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

type pipeProc struct {
	cmd    *exec.Cmd
	stdout *os.File
	stdin  io.WriteCloser
}

func (p *pipeProc) Read(b []byte) (int, error)  { return p.stdout.Read(b) }
func (p *pipeProc) Write(b []byte) (int, error) { return p.stdin.Write(b) }
func (p *pipeProc) Wait() error                 { return p.cmd.Wait() }
func (p *pipeProc) Kill()                       { _ = p.cmd.Process.Kill(); p.stdout.Close() }
func (p *pipeProc) CloseStdin()                 { p.stdin.Close() }
func (p *pipeProc) CloseIO()                    { p.stdout.Close() }
func (p *pipeProc) Resize(cols, rows uint32)    {}

// autoDetectShell 返回系统自动探测的 shell：$SHELL，空则 /bin/sh。
func autoDetectShell() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	return "/bin/sh"
}

// supportPty：Unix 平台用 creack/pty 提供全交互伪终端。
const supportPty = true

// ptyAvailable：Unix 始终可用。
func ptyAvailable() bool { return supportPty }

// startSessionProc 拉起 shell（isShell=true 交互登录）或执行命令行。
// Interactive shells are PTY-only (no line-mode pipe fallback). Command
// execution may use a pipe when no PTY was requested.
func startSessionProc(shell, cmdline string, isShell, usePty bool, cols, rows uint32) (sessionProc, error) {
	workDir := defaultWorkDir()

	if isShell && !usePty {
		return nil, errPTYRequired
	}
	if usePty {
		var cmd *exec.Cmd
		if isShell {
			cmd = exec.Command(shell)
		} else {
			cmd = exec.Command(shell, execArgs(shell, cmdline)...)
		}
		cmd.Dir = workDir
		cmd.Env = sessionEnv()
		f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
		if err != nil {
			return nil, err
		}
		return &ptyProc{cmd: cmd, f: f}, nil
	}

	cmd := exec.Command(shell, execArgs(shell, cmdline)...)
	cmd.Dir = workDir
	cmd.Env = sessionEnv()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		return nil, err
	}
	pw.Close()
	return &pipeProc{cmd: cmd, stdout: pr, stdin: stdin}, nil
}
