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
