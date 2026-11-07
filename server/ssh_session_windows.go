//go:build windows

package server

import (
	"io"
	"os"
	"os/exec"
)

// Windows v1：管道模式（行级交互）。ConPTY 全交互列二期。
// sessionProc 与 ssh_session_unix.go 定义一致，此处重复声明会冲突，
// 故接口定义放在独立的 ssh_session.go 中。

type pipeProc struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stdin  io.WriteCloser
}

func (p *pipeProc) Read(b []byte) (int, error)  { return p.stdout.Read(b) }
func (p *pipeProc) Write(b []byte) (int, error) { return p.stdin.Write(b) }
func (p *pipeProc) Wait() error                  { return p.cmd.Wait() }
func (p *pipeProc) Kill()                        { _ = p.cmd.Process.Kill(); p.stdout.Close() }
func (p *pipeProc) CloseStdin()                  { p.stdin.Close() }
func (p *pipeProc) Resize(cols, rows uint32)     {}

// defaultShell 返回系统默认 shell：%COMSPEC%（默认 cmd.exe）。
func defaultShell() string {
	if sh := os.Getenv("COMSPEC"); sh != "" {
		return sh
	}
	return "cmd.exe"
}

// startSessionProc 拉起 cmd.exe（shell）或 cmd /c 命令行（exec），管道模式。
func startSessionProc(cmdline string, isShell, usePty bool, cols, rows uint32) (sessionProc, error) {
	var cmd *exec.Cmd
	if isShell {
		cmd = exec.Command(defaultShell())
	} else {
		cmd = exec.Command(defaultShell(), "/c", cmdline)
	}
	cmd.Dir = os.Getenv("SystemDrive") + "\\"

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &pipeProc{cmd: cmd, stdout: stdout, stdin: stdin}, nil
}
