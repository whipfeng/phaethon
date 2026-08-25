package config

import (
	"os"
	"testing"
)

func TestLoadEnvFile(t *testing.T) {
	f, err := os.CreateTemp("", "phaethon-env-*.env")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())

	content := `# comment
KEY1=value1
KEY2="value with spaces"
KEY3='single quoted'
export KEY4=exported
EMPTY=
`
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	f.Close()

	loadedEnv = make(map[string]string)
	if err := LoadEnvFile(f.Name()); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		key  string
		want string
	}{
		{"KEY1", "value1"},
		{"KEY2", "value with spaces"},
		{"KEY3", "single quoted"},
		{"KEY4", "exported"},
		{"EMPTY", ""},
	}
	for _, c := range cases {
		if got := loadedEnv[c.key]; got != c.want {
			t.Errorf("loadedEnv[%q] = %q, want %q", c.key, got, c.want)
		}
	}
}

func TestSubstituteEnv(t *testing.T) {
	loadedEnv = map[string]string{
		"HOST": "example.com",
		"PORT": "8080",
	}
	os.Setenv("SYS_VAR", "system")
	defer os.Unsetenv("SYS_VAR")

	cases := []struct {
		input string
		want  string
	}{
		{"server: ${HOST}", "server: example.com"},
		{"port: $PORT", "port: 8080"},
		{"sys: ${SYS_VAR}", "sys: system"},
		{"default: ${MISSING:-fallback}", "default: fallback"},
		{"literal: $${HOST}", "literal: ${HOST}"},
		{"mixed: ${HOST}:${PORT}", "mixed: example.com:8080"},
	}

	for _, c := range cases {
		got := string(SubstituteEnv([]byte(c.input)))
		if got != c.want {
			t.Errorf("SubstituteEnv(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

func TestEnvValuePriority(t *testing.T) {
	loadedEnv = map[string]string{"VAR": "from-env"}
	if got := EnvValue("VAR"); got != "from-env" {
		t.Errorf("EnvValue = %q, want from-env", got)
	}

	os.Setenv("VAR", "from-system")
	defer os.Unsetenv("VAR")
	if got := EnvValue("VAR"); got != "from-system" {
		t.Errorf("EnvValue = %q, want from-system", got)
	}
}
