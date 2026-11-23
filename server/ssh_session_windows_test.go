//go:build windows

package server

import (
	"os"
	"strings"
	"testing"
)

// TestDetectGitBash 校验探测结果只可能是真实存在的 POSIX shell，
// 且绝不会把 System32 的 WSL 启动器当成 Git Bash。
func TestDetectGitBash(t *testing.T) {
	path := detectGitBash()
	if path == "" {
		t.Skip("Git for Windows is not installed on this host")
	}
	if st, err := os.Stat(path); err != nil || st.IsDir() {
		t.Fatalf("detectGitBash() = %q, not an existing file: %v", path, err)
	}
	if classifyShell(path) != shellKindPosix {
		t.Fatalf("detectGitBash() = %q, want a POSIX shell", path)
	}
	if isWSLBash(path) {
		t.Fatalf("detectGitBash() returned the WSL launcher: %q", path)
	}
	if !strings.HasSuffix(strings.ToLower(path), "bash.exe") {
		t.Fatalf("detectGitBash() = %q, want bash.exe", path)
	}
}

// TestAutoDetectShellPrefersGitBash Git Bash 可用时自动探测必须优先选它。
func TestAutoDetectShellPrefersGitBash(t *testing.T) {
	git := detectGitBash()
	if git == "" {
		t.Skip("Git for Windows is not installed on this host")
	}
	if got := autoDetectShell(); got != git {
		t.Fatalf("autoDetectShell() = %q, want Git Bash %q", got, git)
	}
}

// TestIsWSLBash 覆盖 WSL 启动器识别的大小写与分隔符差异。
func TestIsWSLBash(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{`C:\Windows\System32\bash.exe`, true},
		{`c:\windows\system32\bash.exe`, true},
		{`C:/Windows/System32/bash.exe`, true},
		{`C:\Program Files\Git\bin\bash.exe`, false},
		{`C:\Program Files\Git\usr\bin\bash.exe`, false},
	}
	for _, tc := range cases {
		if got := isWSLBash(tc.path); got != tc.want {
			t.Errorf("isWSLBash(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// TestInteractiveShellArgs 编码在启动参数里设置：POSIX 不加参数，cmd 用 /k，
// PowerShell 用 -NoExit -Command；未知程序不注入任何参数（避免 /k 破坏它）。
func TestInteractiveShellArgs(t *testing.T) {
	if args := interactiveShellArgs(`C:\Program Files\Git\bin\bash.exe`); len(args) != 0 {
		t.Fatalf("Git Bash args = %v, want none", args)
	}

	ps := interactiveShellArgs(`C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`)
	psJoined := strings.Join(ps, " ")
	if !strings.Contains(psJoined, "-NoExit") || !strings.Contains(psJoined, "UTF8") {
		t.Fatalf("PowerShell args = %v, want -NoExit and UTF-8 console setup", ps)
	}
	if !strings.Contains(psJoined, "$OutputEncoding=") {
		t.Fatalf("PowerShell args = %v, want $OutputEncoding set for native pipelines", ps)
	}

	cmdArgs := interactiveShellArgs(`C:\Windows\System32\cmd.exe`)
	if len(cmdArgs) != 2 || cmdArgs[0] != "/k" || !strings.Contains(cmdArgs[1], "chcp 65001") {
		t.Fatalf("cmd args = %v, want [/k chcp 65001>nul]", cmdArgs)
	}

	if args := interactiveShellArgs(`C:\tools\myshell.exe`); len(args) != 0 {
		t.Fatalf("unknown shell args = %v, want none", args)
	}

	line := interactiveShellCmdLine(`C:\Windows\System32\cmd.exe`)
	if !strings.HasPrefix(line, `"C:\Windows\System32\cmd.exe" /k `) {
		t.Fatalf("cmdline = %q, want quoted shell followed by /k", line)
	}
}
