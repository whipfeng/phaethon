//go:build windows

package server

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/UserExistsError/conpty"
	"golang.org/x/sys/windows"
)

// Windows：ConPTY 全交互（Win10 1809+），不可用时退回管道模式（行级交互）。
// 探测结果决定 supportPty：管道模式下必须拒绝 pty-req（客户端退回
// cooked 模式才有本地回显与 \n 行结束，否则交互即黑洞）。
var supportPty = conpty.IsConPtyAvailable()

type conptyProc struct {
	cpty *conpty.ConPty
	pid  int
}

type exitCodeError struct{ code uint32 }

func (e *exitCodeError) Error() string { return fmt.Sprintf("exit code %d", e.code) }
func (e *exitCodeError) ExitCode() int { return int(e.code) }

func (p *conptyProc) Read(b []byte) (int, error)  { return p.cpty.Read(b) }
func (p *conptyProc) Write(b []byte) (int, error) { return p.cpty.Write(b) }
func (p *conptyProc) CloseStdin()                 {}
// CloseIO closes the pseudoconsole so a Read blocked after the shell exited
// returns. ConPTY keeps the output pipe open while the object is alive.
func (p *conptyProc) CloseIO() { _ = p.cpty.Close() }
func (p *conptyProc) Resize(cols, rows uint32) {
	_ = p.cpty.Resize(int(cols), int(rows))
}

func (p *conptyProc) Wait() error {
	code, err := p.cpty.Wait(context.Background())
	if err != nil {
		return err
	}
	if code != 0 {
		return &exitCodeError{code: code}
	}
	return nil
}

func (p *conptyProc) Kill() {
	// ConPTY 无 kill API：按 PID 终止进程后关闭伪控制台。
	if h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(p.pid)); err == nil {
		_ = windows.TerminateProcess(h, 1)
		_ = windows.CloseHandle(h)
	}
	_ = p.cpty.Close()
}

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
func (p *pipeProc) CloseIO()                     { p.stdout.Close() }
func (p *pipeProc) Resize(cols, rows uint32)     {}

// autoDetectShell 返回系统自动探测的 shell：pwsh（PowerShell 7，装了优先）→
// powershell.exe → %COMSPEC%（cmd.exe 回退）。
func autoDetectShell() string {
	for _, sh := range []string{"pwsh", "powershell", "cmd"} {
		if path, err := exec.LookPath(sh); err == nil {
			return path
		}
	}
	if sh := os.Getenv("COMSPEC"); sh != "" {
		return sh
	}
	return "cmd.exe"
}

// startSessionProc 拉起 shell（isShell=true 交互登录）或执行命令行。
// usePty 且 ConPTY 可用时走全交互伪终端；否则管道模式（行级交互）。
func startSessionProc(cmdline string, isShell, usePty bool, cols, rows uint32) (sessionProc, error) {
	shell := resolveShell()
	workDir := defaultWorkDir()

	if isShell {
		if usePty && supportPty {
			return startConptyShell(shell, workDir, cols, rows)
		}
		return startPipe(shell, nil, workDir)
	}
	if usePty && supportPty {
		return startConpty("\""+shell+"\" "+strings.Join(quoteArgs(execArgs(shell, cmdline)), " "), workDir, cols, rows)
	}
	return startPipe(shell, execArgs(shell, cmdline), workDir)
}

func startConptyShell(shell, workDir string, cols, rows uint32) (sessionProc, error) {
	// -NoLogo 抑制 PowerShell 启动 banner（它以控制台初始 GBK 代码页输出，
	// 在 chcp 注入执行前就发完了，必然乱码；去掉最干净）。
	commandLine := "\"" + shell + "\""
	if base := strings.ToLower(shell); strings.Contains(base, "powershell") && !strings.Contains(base, "pwsh") {
		commandLine += " -NoLogo"
	}
	p, err := startConpty(commandLine, workDir, cols, rows)
	if err != nil {
		return nil, err
	}
	// ConPTY 输入有队列缓冲，shell 起来后自然会消费到。
	if init := utf8InitCmd(shell); init != "" {
		_, _ = p.Write([]byte(init))
	}
	return p, nil
}

func startConpty(commandLine, workDir string, cols, rows uint32) (*conptyProc, error) {
	opts := []conpty.ConPtyOption{
		conpty.ConPtyDimensions(int(cols), int(rows)),
		conpty.ConPtyWorkDir(workDir),
	}
	cpty, err := conpty.Start(commandLine, opts...)
	if err != nil {
		return nil, err
	}
	return &conptyProc{cpty: cpty, pid: cpty.Pid()}, nil
}

func startPipe(shell string, args []string, workDir string) (sessionProc, error) {
	cmd := exec.Command(shell, args...)
	cmd.Dir = workDir

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
