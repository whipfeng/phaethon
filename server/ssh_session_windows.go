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
	"golang.org/x/sys/windows/registry"
)

// Windows：ConPTY 全交互（Win10 1809+），不可用时用部署目录自带的
// winpty（Win7/1607 等）。交互式 shell 只能走 PTY；两者都不可用时返回
// errPTYUnavailable，不做管道行级回退。
var supportConPTY = conpty.IsConPtyAvailable()

// ptyAvailable reports whether a real PTY can be provided: native ConPTY on
// Win10 1809+, otherwise the bundled winpty.
//
// winpty is enabled on Win7/8 as well: cmd.exe works correctly there (verified
// on Win7 SP1). PowerShell 2.0's ConsoleHost does not work under winpty, but
// shell compatibility is the program's responsibility — a user-specified shell
// is never silently replaced.
func ptyAvailable() bool {
	if supportConPTY {
		return true
	}
	return winptyReady()
}

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
func (p *pipeProc) Wait() error                 { return p.cmd.Wait() }
func (p *pipeProc) Kill()                       { _ = p.cmd.Process.Kill(); p.stdout.Close() }
func (p *pipeProc) CloseStdin()                 { p.stdin.Close() }
func (p *pipeProc) CloseIO()                    { p.stdout.Close() }
func (p *pipeProc) Resize(cols, rows uint32)    {}

// autoDetectShell 返回系统自动探测的 shell：pwsh（PowerShell 7，装了优先）→
// powershell（仅当 Win10+ 或检测到 PowerShell 3+）→ cmd.exe。
//
// Win7/8 只有 PowerShell 2.0 时跳过 powershell：PS2 的 ConsoleHost 在 winpty
// 下无法交互，而 cmd.exe + winpty 已验证可用。用户显式指定的 shell 不受此
// 自动探测影响。
func autoDetectShell() string {
	if path, err := exec.LookPath("pwsh"); err == nil {
		return path
	}
	if windows.RtlGetVersion().MajorVersion >= 10 || hasPowerShell3Plus() {
		if path, err := exec.LookPath("powershell"); err == nil {
			return path
		}
	}
	if path, err := exec.LookPath("cmd"); err == nil {
		return path
	}
	if sh := os.Getenv("COMSPEC"); sh != "" {
		return sh
	}
	return "cmd.exe"
}

// hasPowerShell3Plus checks the WMF 3+ engine key. On Win7, powershell.exe
// stays PS2 unless WMF is installed; the registry key is the cheapest
// non-executing probe.
func hasPowerShell3Plus() bool {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\PowerShell\3\PowerShellEngine`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	_ = key.Close()
	return true
}

// startSessionProc 拉起 shell（isShell=true 交互登录）或执行命令行。
//
// Interactive shells are PTY-only: there is no line-mode pipe fallback.
// Command execution (isShell=false) may use a pipe when no PTY was requested;
// when a PTY was requested it must be real too (no silent fallback).
func startSessionProc(shell, cmdline string, isShell, usePty bool, cols, rows uint32) (sessionProc, error) {
	workDir := defaultWorkDir()

	if isShell {
		if !usePty {
			return nil, errPTYRequired
		}
		// PowerShell 2.0's ConsoleHost cannot enter its input/render loop under
		// winpty (verified on Win7 SP1: no prompt, no echo, `exit` ignored).
		// There is no usable PTY for it, so a user-specified PS2 shell must get
		// an explicit error instead of a black-hole session. cmd.exe is fine.
		if !supportConPTY && windows.RtlGetVersion().MajorVersion < 10 &&
			classifyShell(shell) == shellKindPowerShell && !hasPowerShell3Plus() {
			return nil, fmt.Errorf("PowerShell 2.0 cannot run interactively under winpty; install WMF 5.1 (PowerShell 5.1) or use cmd.exe")
		}
		if supportConPTY {
			return startConptyShell(shell, workDir, cols, rows)
		}
		if winptyReady() {
			return startWinptyShell(shell, workDir, cols, rows)
		}
		return nil, errPTYUnavailable
	}

	if usePty {
		if supportConPTY {
			return startConpty("\""+shell+"\" "+strings.Join(quoteArgs(execArgs(shell, cmdline)), " "), workDir, cols, rows)
		}
		if winptyReady() {
			full := "\"" + shell + "\" " + strings.Join(quoteArgs(execArgs(shell, cmdline)), " ")
			p, err := startWinptyProc(shell, full, workDir, cols, rows, false)
			if err != nil {
				return nil, err
			}
			return p, nil
		}
		return nil, errPTYUnavailable
	}
	return startPipe(shell, execArgs(shell, cmdline), workDir)
}

// startWinptyShell starts an interactive shell inside a winpty PTY. The
// console is switched to UTF-8 with the same init line ConPTY uses; the
// console echoes that line once at session start (cosmetic).
func startWinptyShell(shell, workDir string, cols, rows uint32) (sessionProc, error) {
	cmdline := "\"" + shell + "\""
	if classifyShell(shell) == shellKindPowerShell {
		cmdline += " -NoLogo"
	}
	// PS2 on Win7 does not exit on "exit"/EOF under winpty; the proc
	// terminates it after forwarding exit or when the client disconnects.
	legacy := windows.RtlGetVersion().MajorVersion < 10 &&
		classifyShell(shell) == shellKindPowerShell
	p, err := startWinptyProc(shell, cmdline, workDir, cols, rows, legacy)
	if err != nil {
		return nil, err
	}
	if init := winptyInitCmd(shell); init != "" {
		_, _ = p.Write([]byte(init))
	}
	return p, nil
}

// winptyInitCmd returns the UTF-8 init lines for a winpty console.
//
// The console converts CR to the Enter key (LF becomes a second line break),
// so every line ends with CR only. PS 2.0 (Win7) mangles the long guarded
// one-liner used by ConPTY, so it gets two short lines instead.
func winptyInitCmd(shell string) string {
	switch classifyShell(shell) {
	case shellKindPowerShell:
		if windows.RtlGetVersion().MajorVersion < 10 {
			// Win7 + winpty: chcp 65001 inside the hidden console stalls the
			// PS2 host, so only switch the .NET console encodings here.
			return "[Console]::InputEncoding=[Console]::OutputEncoding=[Text.Encoding]::UTF8\r"
		}
		return strings.TrimRight(utf8InitCmd(shell), "\r\n") + "\r"
	case shellKindCmd:
		return "chcp 65001 >nul\r"
	}
	return ""
}

func startConptyShell(shell, workDir string, cols, rows uint32) (sessionProc, error) {
	// -NoLogo 抑制 PowerShell 启动 banner（它以控制台初始 GBK 代码页输出，
	// 在 chcp 注入执行前就发完了，必然乱码；去掉最干净）。
	commandLine := "\"" + shell + "\""
	if classifyShell(shell) == shellKindPowerShell {
		// -NoLogo 同时适用于 Windows PowerShell 5.1 与 pwsh 7：banner 在
		// 控制台初始代码页下输出，晚于 chcp 注入，必然乱码。
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
	cmd.Env = sessionEnv()

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
