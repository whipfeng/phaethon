package server

import (
	"reflect"
	"testing"

	"phaethon/db"
)

// setupShellTestDB 每个用例独立临时库；db.Init 会覆盖全局句柄，必须成对 Close。
func setupShellTestDB(t *testing.T) {
	t.Helper()
	if err := db.Init(t.TempDir() + "/test.db"); err != nil {
		t.Fatalf("db.Init: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
}

// TestResolveShellDBOverride 配置了 shell 时必须优先于自动探测。
func TestResolveShellDBOverride(t *testing.T) {
	setupShellTestDB(t)

	const want = "/usr/bin/zsh"
	if err := db.PutSSHConfig(&db.SSHConfig{Enabled: true, Shell: want}); err != nil {
		t.Fatalf("PutSSHConfig: %v", err)
	}

	if got := resolveShell(); got != want {
		t.Errorf("resolveShell() = %q, want %q (db override must win)", got, want)
	}
}

// TestResolveShellFallback 空 shell 时必须回退到平台自动探测，保持存量行为。
func TestResolveShellFallback(t *testing.T) {
	setupShellTestDB(t)

	if err := db.PutSSHConfig(&db.SSHConfig{Enabled: true, Shell: ""}); err != nil {
		t.Fatalf("PutSSHConfig: %v", err)
	}

	want := autoDetectShell()
	if got := resolveShell(); got != want {
		t.Errorf("resolveShell() = %q, want autoDetectShell() = %q", got, want)
	}
}

// TestResolveShellChoiceSource 区分“用户显式指定”和“自动探测”，供
// handleSession 决定是否允许回退到内置 shell。
func TestResolveShellChoiceSource(t *testing.T) {
	setupShellTestDB(t)

	if err := db.PutSSHConfig(&db.SSHConfig{Enabled: true, Shell: "/usr/bin/zsh"}); err != nil {
		t.Fatalf("PutSSHConfig: %v", err)
	}
	shell, explicit := resolveShellChoice()
	if !explicit || shell != "/usr/bin/zsh" {
		t.Fatalf("explicit choice = (%q, %v), want (/usr/bin/zsh, true)", shell, explicit)
	}

	if err := db.PutSSHConfig(&db.SSHConfig{Enabled: true, Shell: ""}); err != nil {
		t.Fatalf("PutSSHConfig: %v", err)
	}
	shell, explicit = resolveShellChoice()
	if explicit {
		t.Fatalf("empty config marked as explicit")
	}
	if shell != autoDetectShell() {
		t.Fatalf("auto choice = %q, want %q", shell, autoDetectShell())
	}
}

// TestExecArgsBash 校验不同 shell 家族的参数拼接：
// POSIX → -c，PowerShell → -Command，其余 → /s /c。
func TestExecArgsBash(t *testing.T) {
	cases := []struct {
		name    string
		shell   string
		cmdline string
		want    []string
	}{
		{"bash absolute", "/bin/bash", "ls -l", []string{"-c", "ls -l"}},
		{"bash windows path", `C:\Program Files\Git\bin\bash.exe`, "ls", []string{"-c", "ls"}},
		{"zsh", "/usr/bin/zsh", "echo hi", []string{"-c", "echo hi"}},
		{"plain sh", "sh", "ls", []string{"-c", "ls"}},
		{"fish", "/usr/bin/fish", "ls", []string{"-c", "ls"}},
		{"pwsh", "pwsh", "Get-Date", []string{"-Command", "Get-Date"}},
		{"powershell exe", "powershell.exe", "Get-Date", []string{"-Command", "Get-Date"}},
		{"cmd exe", "cmd.exe", "dir", []string{"/s", "/c", "dir"}},
		{"cmd fallback", "unknown-shell", "dir", []string{"/s", "/c", "dir"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := execArgs(tc.shell, tc.cmdline)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("execArgs(%q, %q) = %v, want %v", tc.shell, tc.cmdline, got, tc.want)
			}
		})
	}
}

// TestClassifyShell 覆盖分类边界：大小写、.exe/.bat 后缀、sh 结尾判定。
func TestClassifyShell(t *testing.T) {
	cases := []struct {
		shell string
		want  shellKind
	}{
		{"/bin/bash", shellKindPosix},
		{`C:\Program Files\Git\bin\BASH.EXE`, shellKindPosix},
		{"/usr/bin/zsh", shellKindPosix},
		{"pwsh.exe", shellKindPowerShell},
		{"PowerShell.exe", shellKindPowerShell},
		{"cmd.exe", shellKindCmd},
		{"command.com", shellKindCmd},
	}

	for _, tc := range cases {
		if got := classifyShell(tc.shell); got != tc.want {
			t.Errorf("classifyShell(%q) = %v, want %v", tc.shell, got, tc.want)
		}
	}
}
