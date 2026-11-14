package server

import (
	"strings"
	"testing"
)

func TestSplitCommand(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"ls -la", []string{"ls", "-la"}},
		{`cat "a b.txt"`, []string{"cat", "a b.txt"}},
		{`cat 'a b.txt'`, []string{"cat", "a b.txt"}},
		{`echo a\ b`, []string{"echo", "a b"}},
		{"  ", nil},
	}
	for _, c := range cases {
		got := splitCommand(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("splitCommand(%q) = %v, want %v", c.in, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("splitCommand(%q) = %v, want %v", c.in, got, c.want)
			}
		}
	}
}

func TestBuiltinShellBasicCommands(t *testing.T) {
	m := newBuiltinShellModel(80, 24, "test reason")

	if out, _ := m.execute("pwd"); strings.TrimSpace(out) == "" {
		t.Fatal("pwd returned empty")
	}
	if out, _ := m.execute("help"); !strings.Contains(out, "内置命令") {
		t.Fatalf("help output = %q", out)
	}
	if out, _ := m.execute("cd ."); out != "" {
		t.Fatalf("cd . returned error: %q", out)
	}
	if out, _ := m.execute("no-such-command"); !strings.Contains(out, "未知命令") {
		t.Fatalf("unknown command output = %q", out)
	}
	if out, _ := m.execute("!echo builtin-shell-test"); !strings.Contains(out, "builtin-shell-test") {
		t.Fatalf("!echo output = %q", out)
	}
}
