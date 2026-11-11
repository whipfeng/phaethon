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

	"phaethon/util"
)

// Windows：ConPTY 全交互（Win10 1809+），不可用时用部署目录自带的
// winpty（Win7/1607 等）；两者都没有时退回管道模式（行级交互）。
// 探测结果决定 ptyAvailable：管道模式下必须拒绝 pty-req（客户端退回
// cooked 模式才有本地回显与 \n 行结束，否则交互即黑洞）。
var supportConPTY = conpty.IsConPtyAvailable()

// ptyAvailable reports whether a real PTY can be provided: native ConPTY on
// Win10 1809+, otherwise the bundled winpty (Win10 only).
//
// Win7/8 are excluded even though winpty supports the OS: PowerShell 2.0's
// interactive host under winpty is unreliable (sometimes renders nothing and
// exits immediately, and does not exit on "exit"), while the line-mode pipe
// fallback is stable there. Installing WMF 5.1 / PowerShell 7 on such nodes
// would make winpty viable.
// ptyAvailable reports whether a real PTY can be provided: native ConPTY on
// Win10 1809+, otherwise the bundled winpty (Win10 only).
//
// Win7/8 are excluded: winpty's agent renders by scraping the hidden console's
// screen buffer, and on qgw (Win7) the desktop session is usually disconnected
// (qwinsta: wangqian = Disc). Input is still delivered and the shell executes
// commands, but conout yields only the initial clear, so the SSH session looks
// empty. With an active session it works, but an unattended node cannot rely
// on that. Win10 nodes use ConPTY instead when available.
func ptyAvailable() bool {
	if supportConPTY {
		return true
	}
	if windows.RtlGetVersion().MajorVersion < 10 {
		return false
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
		if usePty {
			if supportConPTY {
				return startConptyShell(shell, workDir, cols, rows)
			}
			if winptyReady() {
				if p, err := startWinptyShell(shell, workDir, cols, rows); err == nil {
					return p, nil
				} else {
					util.LogModuleWarn("ssh", "winpty shell start failed, fallback to pipe: %v", err)
				}
			}
		}
		args := interactiveShellArgs(shell)
		p, err := startPipe(shell, args, workDir)
		if err != nil {
			return nil, err
		}
		// 管道模式没有真实终端：PowerShell 启动 banner 在切换编码前就以
		// 控制台初始代码页（中文系统 GBK）输出，UTF-8 客户端必然乱码，用
		// -NoLogo 去掉；再把输出编码切到 UTF-8。注入写在本会话复制用户
		// 输入之前，顺序有保证，且失败时静默（无控制台句柄时 chcp 会报错）。
		if init := utf8InitCmdPipe(shell); init != "" {
			_, _ = p.Write([]byte(init))
		}
		// PS 2.0 的 "-Command -" 宿主总在输出首部写 UTF-8 BOM；只对
		// PowerShell 包一层剥离，cmd 的原始输出不受影响。
		if classifyShell(shell) == shellKindPowerShell {
			return newBOMStripProc(p), nil
		}
		return p, nil
	}
	if usePty {
		if supportConPTY {
			return startConpty("\""+shell+"\" "+strings.Join(quoteArgs(execArgs(shell, cmdline)), " "), workDir, cols, rows)
		}
		if winptyReady() {
			full := "\"" + shell + "\" " + strings.Join(quoteArgs(execArgs(shell, cmdline)), " ")
			if p, err := startWinptyProc(shell, full, workDir, cols, rows, false); err == nil {
				return p, nil
			}
		}
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
			return "[Console]::InputEncoding=[Console]::OutputEncoding=[Text.Encoding]::UTF8\r"
		}
		return strings.TrimRight(utf8InitCmd(shell), "\r\n") + "\r"
	case shellKindCmd:
		return "chcp 65001 >nul\r"
	}
	return ""
}

// interactiveShellArgs 返回管道模式下交互 shell 的参数。
//
// 无 ConPTY 时没有真正的 PTY，但 Windows 10+ 自带的 PowerShell 3.0+ 交互
// 宿主（-NoLogo，无 -Command）可以正常读取重定向 stdin 并打印提示符；
// Win7/8 的 PS 2.0 不带 "-Command -" 时根本不读 stdin（交互直接挂起），
// 只能用 -Command -（能执行命令，但没有服务端提示符）。
// cmd 用 /Q 关闭命令回显。
func interactiveShellArgs(shell string) []string {
	switch classifyShell(shell) {
	case shellKindPowerShell:
		if windows.RtlGetVersion().MajorVersion < 10 {
			return []string{"-NoLogo", "-Command", "-"}
		}
		return []string{"-NoLogo"}
	case shellKindCmd:
		return []string{"/Q"}
	}
	return nil
}

// bomStripProc removes a leading UTF-8 BOM from the first output chunk.
// PS 2.0's "-Command -" host writes one even when Console.OutputEncoding is a
// no-BOM encoder, which UTF-8 clients otherwise render as a stray character.
// We strip it inline instead of using bufio.Peek so a short first read never
// delays the session output.
type bomStripProc struct {
	sessionProc
	checked bool
}

func newBOMStripProc(p sessionProc) *bomStripProc {
	return &bomStripProc{sessionProc: p}
}

func (p *bomStripProc) Read(b []byte) (int, error) {
	n, err := p.sessionProc.Read(b)
	if !p.checked && (n > 0 || err != nil) {
		p.checked = true
		if n >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
			copy(b[0:n-3], b[3:n])
			n -= 3
		}
	}
	return n, err
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
