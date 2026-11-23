//go:build windows

package server

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

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

	// mu guards closed: conpty.ConPty.Close() must run exactly once. Calling it
	// twice (CloseIO in the grace path, then the deferred Kill) corrupts the
	// heap inside the library and takes the whole worker down.
	mu     sync.Mutex
	closed bool
}

type exitCodeError struct{ code uint32 }

func (e *exitCodeError) Error() string { return fmt.Sprintf("exit code %d", e.code) }
func (e *exitCodeError) ExitCode() int { return int(e.code) }

func (p *conptyProc) Read(b []byte) (int, error)  { return p.cpty.Read(b) }
func (p *conptyProc) Write(b []byte) (int, error) { return p.cpty.Write(b) }
func (p *conptyProc) CloseStdin()                 {}

// CloseIO closes the pseudoconsole so a Read blocked after the shell exited
// returns. ConPTY keeps the output pipe open while the object is alive.
func (p *conptyProc) CloseIO() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closeLocked()
}

// closeLocked closes the pseudoconsole at most once. Callers must hold p.mu.
func (p *conptyProc) closeLocked() {
	if p.closed {
		return
	}
	p.closed = true
	_ = p.cpty.Close()
}

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
	p.mu.Lock()
	defer p.mu.Unlock()
	// ClosePseudoConsole already terminates the attached shell. After the pty
	// was closed, this PID may already belong to an unrelated process, so never
	// terminate by a recycled PID.
	if !p.closed {
		// ConPTY 无 kill API：按 PID 终止进程后关闭伪控制台。
		if h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(p.pid)); err == nil {
			_ = windows.TerminateProcess(h, 1)
			_ = windows.CloseHandle(h)
		}
	}
	p.closeLocked()
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

// autoDetectShell 返回系统自动探测的 shell：Git Bash（POSIX）→ pwsh
// （PowerShell 7，装了优先）→ powershell（仅当 Win10+ 或检测到 PowerShell 3+）
// → cmd.exe。
//
// Git Bash 排在最前：msys2 的 bash 是真正的 POSIX 交互 shell，在 ConPTY 与
// winpty 下都有稳定的输入回显循环，且原生 UTF-8（无需 chcp 注入）。Windows
// OpenSSH 的 DefaultShell 配成 Git Bash 时，系统 ssh 与内置 ssh 的行为就一致。
//
// Win7/8 只有 PowerShell 2.0 时跳过 powershell：PS2 的 ConsoleHost 在 winpty
// 下无法交互，而 cmd.exe + winpty 已验证可用。用户显式指定的 shell 不受此
// 自动探测影响。
func autoDetectShell() string {
	if path := detectGitBash(); path != "" {
		return path
	}
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

// detectGitBash 返回 Git for Windows 安装目录下的 bash.exe，找不到返回空串。
//
// 依次探测：官方安装器写入的注册表 InstallPath（HKLM/HKCU）→ 常见安装目录 →
// PATH。PATH 上的 System32\bash.exe 是 WSL 启动器（拉起 Linux shell），不是
// Git Bash，需排除。
func detectGitBash() string {
	var candidates []string
	for _, root := range []registry.Key{registry.LOCAL_MACHINE, registry.CURRENT_USER} {
		k, err := registry.OpenKey(root, `SOFTWARE\GitForWindows`, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		if install, _, err := k.GetStringValue("InstallPath"); err == nil && install != "" {
			candidates = append(candidates, filepath.Join(install, "bin", "bash.exe"))
		}
		_ = k.Close()
	}
	for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "ProgramW6432"} {
		if dir := os.Getenv(env); dir != "" {
			candidates = append(candidates, filepath.Join(dir, "Git", "bin", "bash.exe"))
		}
	}
	if dir := os.Getenv("LOCALAPPDATA"); dir != "" {
		// 用户级安装（安装器默认不勾选“仅为我安装”，但仍可能被选中）。
		candidates = append(candidates, filepath.Join(dir, "Programs", "Git", "bin", "bash.exe"))
	}
	if path, err := exec.LookPath("bash"); err == nil && !isWSLBash(path) {
		candidates = append(candidates, path)
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}

// isWSLBash reports whether path is the System32 WSL bash launcher, which
// starts a Linux shell rather than Git Bash.
func isWSLBash(path string) bool {
	return strings.Contains(strings.ToLower(filepath.ToSlash(path)), "/windows/system32/")
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

// interactiveShellCmdLine builds the CreateProcess command line for an
// interactive shell, including the startup arguments that put its console into
// UTF-8.
//
// Encoding is selected at process startup instead of being written into the PTY
// after the shell is running: an injected line is echoed once at session start
// (both ConPTY and winpty display it) and races the shell's first output.
// POSIX shells (Git Bash etc.) are UTF-8 native and need nothing.
func interactiveShellCmdLine(shell string) string {
	parts := []string{"\"" + shell + "\""}
	parts = append(parts, quoteArgs(interactiveShellArgs(shell))...)
	return strings.Join(parts, " ")
}

// interactiveShellArgs returns the UTF-8 startup arguments for an interactive
// shell.
func interactiveShellArgs(shell string) []string {
	switch classifyShell(shell) {
	case shellKindPowerShell:
		// $OutputEncoding covers the pipeline into native programs (PowerShell
		// 5.1 defaults it to ASCII); the [Console] pair covers reading native
		// output and the interactive input buffer.
		const setUTF8 = "[Console]::InputEncoding=[Console]::OutputEncoding=[Text.Encoding]::UTF8; $OutputEncoding=[Text.Encoding]::UTF8"
		if windows.RtlGetVersion().MajorVersion < 10 {
			// Win7 (WMF 3+) stalls when chcp runs inside the startup command;
			// switching the .NET console/pipe encodings is enough there.
			return []string{"-NoLogo", "-NoExit", "-Command", setUTF8}
		}
		// PS 5.1 caches the console code page at startup, so chcp alone is not
		// enough — native output still needs the console at 65001. -NoLogo
		// suppresses the banner, which is printed in the initial code page and
		// would always be garbled.
		return []string{"-NoLogo", "-NoExit", "-Command", "chcp 65001 > $null; " + setUTF8}
	case shellKindCmd:
		if !isRealCmd(shell) {
			// classifyShell treats unknown programs as cmd, but /k would be
			// meaningless (or harmful) for them. Only pass arguments to the
			// program when it is actually cmd.exe.
			return nil
		}
		return []string{"/k", "chcp 65001>nul"}
	}
	return nil
}

// isRealCmd reports whether shell resolves to cmd.exe rather than an arbitrary
// program that classifyShell fell back to.
func isRealCmd(shell string) bool {
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(shell)), ".exe")
	return base == "cmd"
}

// startWinptyShell starts an interactive shell inside a winpty PTY.
func startWinptyShell(shell, workDir string, cols, rows uint32) (sessionProc, error) {
	// PS on Win7 does not exit on "exit"/EOF under winpty; the proc terminates
	// it after forwarding exit or when the client disconnects.
	legacy := windows.RtlGetVersion().MajorVersion < 10 &&
		classifyShell(shell) == shellKindPowerShell
	return startWinptyProc(shell, interactiveShellCmdLine(shell), workDir, cols, rows, legacy)
}

func startConptyShell(shell, workDir string, cols, rows uint32) (sessionProc, error) {
	return startConpty(interactiveShellCmdLine(shell), workDir, cols, rows)
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
