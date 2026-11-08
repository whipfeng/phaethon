package db

import (
	"testing"

	bolt "go.etcd.io/bbolt"
)

// setupSSHTestDB 每个用例独立临时库，避免跨用例串状态。
func setupSSHTestDB(t *testing.T) {
	t.Helper()
	if err := Init(t.TempDir() + "/test.db"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = Close() })
}

// TestSSHConfigWithShell 验证 Shell 字段写入/读取往返。
func TestSSHConfigWithShell(t *testing.T) {
	setupSSHTestDB(t)

	want := &SSHConfig{
		Enabled: true,
		Shell:   `C:\Program Files\Git\bin\bash.exe`,
	}
	if err := PutSSHConfig(want); err != nil {
		t.Fatalf("PutSSHConfig: %v", err)
	}

	got, err := GetSSHConfig()
	if err != nil {
		t.Fatalf("GetSSHConfig: %v", err)
	}
	if got.Enabled != want.Enabled {
		t.Errorf("Enabled = %v, want %v", got.Enabled, want.Enabled)
	}
	if got.Shell != want.Shell {
		t.Errorf("Shell = %q, want %q", got.Shell, want.Shell)
	}
}

// TestSSHConfigLegacyNoShell 旧版本只存 {"enabled":...}，没有 shell 字段；
// 反序列化后 Shell 必须为零值，从而走自动探测，不破坏存量节点行为。
func TestSSHConfigLegacyNoShell(t *testing.T) {
	setupSSHTestDB(t)

	legacy := []byte(`{"enabled":true}`)
	if err := Update(func(tx *bolt.Tx) error {
		return tx.Bucket(BucketSSH).Put(sshConfigItem, legacy)
	}); err != nil {
		t.Fatalf("write legacy config: %v", err)
	}

	got, err := GetSSHConfig()
	if err != nil {
		t.Fatalf("GetSSHConfig: %v", err)
	}
	if !got.Enabled {
		t.Errorf("Enabled = false, want true (legacy value must survive)")
	}
	if got.Shell != "" {
		t.Errorf("Shell = %q, want empty (missing field must decode to zero)", got.Shell)
	}
}

// TestSSHConfigNotFoundDefaultsEnabled 未配置时默认启用（无公钥则无人可登录）。
func TestSSHConfigNotFoundDefaultsEnabled(t *testing.T) {
	setupSSHTestDB(t)

	got, err := GetSSHConfig()
	if err != nil {
		t.Fatalf("GetSSHConfig: %v", err)
	}
	if !got.Enabled {
		t.Errorf("Enabled = false, want true (default)")
	}
	if got.Shell != "" {
		t.Errorf("Shell = %q, want empty", got.Shell)
	}
}
