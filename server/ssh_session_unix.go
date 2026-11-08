//go:build !windows

package server

import (
	"io"
	"os"
	"os/exec"

	"github.com/creack/pty"
)

type ptyProc struct {
	cmd *exec.Cmd
	f   *os.File
}

func (p *ptyProc) Read(b []byte) (int, error)  { return p.f.Read(b) }
func (p *ptyProc) Write(b []byte) (int, error) { return p.f.Write(b) }
func (p *ptyProc) Wait() error                  { return p.cmd.Wait() }
func (p *ptyProc) Kill()                        { _ = p.cmd.Process.Kill(); p.f.Close() }
func (p *ptyProc) CloseStdin()                  {}
func (p *ptyProc) Resize(cols, rows uint32) {
	_ = pty.Setsize(p.f, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

type pipeProc struct {
	cmd    *exec.Cmd
	stdout *os.File
	stdin  io.WriteCloser
}

func (p *pipeProc) Read(b []byte) (int, error)  { return p.stdout.Read(b) }
func (p *pipeProc) Write(b []byte) (int, error) { return p.stdin.Write(b) }
func (p *pipeProc) Wait() error                  { return p.cmd.Wait() }
func (p *pipeProc) Kill()                        { _ = p.cmd.Process.Kill(); p.stdout.Close() }
func (p *pipeProc) CloseStdin()                  { p.stdin.Close() }
func (p *pipeProc) Resize(cols, rows uint32)     {}

// autoDetectShell 返回系统自动探测的 shell：$SHELL，空则 /bin/sh。
func autoDetectShell() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	return "/bin/sh"
}

// supportPty：Unix 平台用 creack/pty 提供全交互伪终端。
const supportPty = true

// startSessionProc 拉起 shell（isShell=true 交互登录）或执行命令行。
// usePty 时用 pty 全交互（转发窗口尺寸）；否则管道模式。
func startSessionProc(cmdline string, isShell, usePty bool, cols, rows uint32) (sessionProc, error) {
	shell := resolveShell()
	workDir := defaultWorkDir()

	var cmd *exec.Cmd
	if isShell {
		cmd = exec.Command(shell)
	} else {
		cmd = exec.Command(shell, execArgs(shell, cmdline)...)
	}
	cmd.Dir = workDir

	if usePty {
		f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
		if err != nil {
			return nil, err
		}
		return &ptyProc{cmd: cmd, f: f}, nil
	}

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
