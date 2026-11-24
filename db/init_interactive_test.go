package db

import (
	"bufio"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// TestInteractiveInitAsksOneQuestionAtATime guarantees the wizard consumes
// exactly one answer per numbered question and persists what was entered.
// The old failure mode was silent: prompts were emitted into a line-oriented
// pipe, never displayed, and every ReadString error was ignored.
func TestInteractiveInitAsksOneQuestionAtATime(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	defer Close()

	// answer 1: default bind, 2: default port, 3: no auth, 4: explicit node id.
	in := strings.NewReader("\n\n\nint-test-node\n")
	out := new(strings.Builder)

	if err := interactiveInit(bufio.NewReader(in), out, false); err != nil {
		t.Fatalf("interactiveInit failed: %v\noutput:\n%s", err, out.String())
	}

	admin, err := GetAdminConfig()
	if err != nil {
		t.Fatalf("GetAdminConfig failed: %v", err)
	}
	if admin == nil {
		t.Fatal("admin config not stored")
	}
	if admin.Addr != "127.0.0.1:39999" {
		t.Fatalf("admin addr = %q, want 127.0.0.1:39999", admin.Addr)
	}
	if admin.AuthEnabled {
		t.Fatal("auth must stay disabled when the answer is empty")
	}

	mesh, err := GetMeshConfig()
	if err != nil {
		t.Fatalf("GetMeshConfig failed: %v", err)
	}
	if mesh == nil || mesh.NodeID != "int-test-node" {
		t.Fatalf("mesh node id = %v, want int-test-node", mesh)
	}

	printed := out.String()
	for _, want := range []string{"[1/4]", "[2/4]", "[3/4]", "[4/4]"} {
		if !strings.Contains(printed, want) {
			t.Fatalf("output missing %s:\n%s", want, printed)
		}
	}
}

// TestInteractiveInitAbortsOnEOF ensures a closed stdin aborts the wizard
// instead of silently accepting defaults.
func TestInteractiveInitAbortsOnEOF(t *testing.T) {
	if err := interactiveInit(bufio.NewReader(strings.NewReader("")), io.Discard, false); err == nil {
		t.Fatal("expected error on immediate EOF")
	} else if !strings.Contains(err.Error(), "输入流已结束") {
		t.Fatalf("unexpected error: %v", err)
	}

	// Answer the first question, then EOF: must still abort before writing.
	if err := interactiveInit(bufio.NewReader(strings.NewReader("1\n")), io.Discard, false); err == nil {
		t.Fatal("expected error on EOF after the first answer")
	} else if !strings.Contains(err.Error(), "输入流已结束") {
		t.Fatalf("unexpected error: %v", err)
	}
}
