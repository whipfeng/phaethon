package server

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"phaethon/db"
	"phaethon/util"
)

// sessionProc 抽象被拉起进程的双向管道与生命周期。
// 具体实现在 ssh_session_unix.go（pty/管道）与 ssh_session_windows.go（管道）。
type sessionProc interface {
	io.Reader
	io.Writer
	Wait() error
	Kill()
	CloseStdin()
	// CloseIO closes the read side (pty master / ConPTY / stdout pipe) without
	// signalling the process. Used after Wait() to release a reader that would
	// otherwise block forever once the shell exits.
	CloseIO()
	Resize(cols, rows uint32)
}

// resolveShell 优先取 db 配置的 shell；未配置时走各平台 autoDetectShell。
// db 读失败且非 ErrNotFound 时打 WARN，便于排查"我配的 shell 为啥没生效"。
func resolveShell() string {
	cfg, err := db.GetSSHConfig()
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		util.LogModuleWarn("ssh", "GetSSHConfig fail, fallback to autoDetect: %v", err)
	}
	if err == nil && cfg.Shell != "" {
		return cfg.Shell
	}
	return autoDetectShell()
}

// shellKind 对 shell 路径分类：posix(bash/sh/zsh)、powershell、cmd。
// 判定按 basename 不含扩展名做，含大小写不敏感匹配。
type shellKind int

const (
	shellKindCmd shellKind = iota
	shellKindPowerShell
	shellKindPosix
)

func classifyShell(shell string) shellKind {
	base := strings.ToLower(filepath.Base(shell))
	base = strings.TrimSuffix(base, ".exe")
	base = strings.TrimSuffix(base, ".bat")
	base = strings.TrimSuffix(base, ".com")
	if strings.Contains(base, "powershell") || base == "pwsh" {
		return shellKindPowerShell
	}
	if strings.Contains(base, "bash") || strings.Contains(base, "zsh") ||
		strings.Contains(base, "fish") || strings.Contains(base, "ksh") ||
		base == "sh" || strings.HasSuffix(base, "sh") {
		return shellKindPosix
	}
	return shellKindCmd
}

// execArgs 返回 shell 执行外部命令行的参数：cmd 用 /s /c，PowerShell 用 -Command，
// POSIX 系（bash/sh/zsh）用 -c。
func execArgs(shell, cmdline string) []string {
	switch classifyShell(shell) {
	case shellKindPowerShell:
		return []string{"-Command", cmdline}
	case shellKindPosix:
		return []string{"-c", cmdline}
	default:
		return []string{"/s", "/c", cmdline}
	}
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

// utf8InitCmd 是 ConPTY shell 会话启动时注入的命令：把控制台切到 UTF-8，
// 消除客户端按 UTF-8 解码时的中文乱码。POSIX 系无需 chcp，返回空串。
// 仅 Windows 使用；Unix 平台 pty 没有 chcp 概念。
func utf8InitCmd(shell string) string {
	switch classifyShell(shell) {
	case shellKindPowerShell:
		// PS 5.1 启动时缓存控制台代码页，chcp 后还需重置 Console 编码。
		// PS 的 >nul 是 Out-File 到文件 nul（会报错），置空要用 | Out-Null。
		return "chcp 65001 | Out-Null; [Console]::InputEncoding=[Console]::OutputEncoding=[Text.Encoding]::UTF8\r\n"
	case shellKindPosix:
		return ""
	default:
		return "chcp 65001 >nul\r\n"
	}
}

// defaultWorkDir 返回新会话的工作目录：优先用户 home，失败回退到根目录。
func defaultWorkDir() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return home
	}
	if os.PathSeparator == '\\' {
		if sd := os.Getenv("SystemDrive"); sd != "" {
			return sd + "\\"
		}
	}
	return string(os.PathSeparator)
}

// ValidateShellPath 校验 shell 可执行：绝对路径看文件是否存在；相对名走 LookPath。
// 返回错误仅作 admin UI 警告，不阻止保存（用户可能配未来要装的路径）。
func ValidateShellPath(shell string) error {
	if shell == "" {
		return nil
	}
	if filepath.IsAbs(shell) || strings.ContainsRune(shell, os.PathSeparator) || strings.Contains(shell, "/") || strings.Contains(shell, "\\") {
		if _, err := os.Stat(shell); err != nil {
			return err
		}
		return nil
	}
	_, err := exec.LookPath(shell)
	return err
}
