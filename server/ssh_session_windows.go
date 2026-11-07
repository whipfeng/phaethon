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
func (p *pipeProc) Resize(cols, rows uint32)     {}

// defaultShell 返回系统默认 shell：pwsh（PowerShell 7，装了优先）→
// powershell.exe → %COMSPEC%（cmd.exe 回退）。
func defaultShell() string {
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

// execArgs 返回 shell 执行外部命令行的参数：cmd 用 /s /c，PowerShell 用 -Command。
func execArgs(shell, cmdline string) []string {
	base := strings.ToLower(shell)
	if strings.Contains(base, "powershell") || strings.Contains(base, "pwsh") {
		return []string{"-Command", cmdline}
	}
	return []string{"/s", "/c", cmdline}
}

// utf8InitCmd 是 ConPTY shell 会话启动时注入的命令：把控制台切到 UTF-8，
// 消除客户端按 UTF-8 解码时的中文乱码。
func utf8InitCmd(shell string) string {
	base := strings.ToLower(shell)
	if strings.Contains(base, "powershell") || strings.Contains(base, "pwsh") {
		// PS 5.1 启动时缓存控制台代码页，chcp 后还需重置 Console 编码。
		// PS 的 >nul 是 Out-File 到文件 nul（会报错），置空要用 | Out-Null。
		return "chcp 65001 | Out-Null; [Console]::InputEncoding=[Console]::OutputEncoding=[Text.Encoding]::UTF8\r\n"
	}
	return "chcp 65001 >nul\r\n"
}

// startSessionProc 拉起 shell（isShell=true 交互登录）或执行命令行。
// usePty 且 ConPTY 可用时走全交互伪终端；否则管道模式（行级交互）。
func startSessionProc(cmdline string, isShell, usePty bool, cols, rows uint32) (sessionProc, error) {
	shell := defaultShell()
	workDir := os.Getenv("SystemDrive") + "\\"

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

// quoteArgs 给含空格的参数加引号，拼 ConPTY 命令行用。
func quoteArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if strings.ContainsAny(a, " \t\"") {
			out[i] = "\"" + strings.ReplaceAll(a, "\"", "\\\"") + "\""
		} else {
			out[i] = a
		}
	}
	return out
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
	_, _ = p.Write([]byte(utf8InitCmd(shell)))
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
