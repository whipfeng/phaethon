package config

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// envVarPattern matches ${VAR}, ${VAR:-default}, and $VAR.
var envVarPattern = regexp.MustCompile(`\$\{(?P<name>[^{}:\s]+)(?::-(?P<default>[^}]*))?\}|\$(?P<simple>[[:alpha:]_][[:alnum:]_]*)`)

// loadedEnv holds values parsed from the .env file.
// It is populated once at startup and used as a fallback for os.Getenv.
var loadedEnv = make(map[string]string)

// LoadEnvFile parses a .env file and stores its values.
// It does not clear previously loaded values, so it can be called multiple times.
func LoadEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open .env file fail: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Strip optional "export " prefix.
		line = strings.TrimPrefix(line, "export ")

		key, value, ok := splitEnvLine(line)
		if !ok {
			continue
		}
		loadedEnv[key] = value
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read .env file fail: %w", err)
	}
	return nil
}

// splitEnvLine parses a single KEY=VALUE line.
// It supports unquoted, double-quoted, and single-quoted values.
func splitEnvLine(line string) (string, string, bool) {
	idx := strings.Index(line, "=")
	if idx <= 0 {
		return "", "", false
	}

	key := strings.TrimSpace(line[:idx])
	value := strings.TrimSpace(line[idx+1:])

	if len(value) >= 2 {
		switch {
		case strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`):
			value = value[1 : len(value)-1]
			value = strings.ReplaceAll(value, `\"`, `"`)
			value = strings.ReplaceAll(value, `\n`, "\n")
			value = strings.ReplaceAll(value, `\t`, "\t")
		case strings.HasPrefix(value, `'`) && strings.HasSuffix(value, `'`):
			value = value[1 : len(value)-1]
		}
	}

	return key, value, true
}

// EnvValue returns the value of an environment variable.
// System environment variables take precedence over .env values.
func EnvValue(name string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return loadedEnv[name]
}

// escapedVarMarker is used to temporarily hide $${ sequences from the regex.
const escapedVarMarker = "\x00PHAETHON_ESCAPED_VAR\x00"

// SubstituteEnv replaces environment variable references in data.
// Supported forms: ${VAR}, ${VAR:-default}, $VAR, and escaped $${VAR}.
func SubstituteEnv(data []byte) []byte {
	// Hide escaped forms so they are not matched by ${VAR}.
	data = bytes.ReplaceAll(data, []byte("$${"), []byte(escapedVarMarker+"{"))

	data = envVarPattern.ReplaceAllFunc(data, func(match []byte) []byte {
		s := string(match)

		// ${VAR} or ${VAR:-default}
		if strings.HasPrefix(s, "${") {
			inner := s[2 : len(s)-1]
			name := inner
			defaultValue := ""
			if idx := strings.Index(inner, ":-"); idx >= 0 {
				name = inner[:idx]
				defaultValue = inner[idx+2:]
			}
			if v := EnvValue(name); v != "" {
				return []byte(v)
			}
			return []byte(defaultValue)
		}

		// $VAR
		name := s[1:]
		if v := EnvValue(name); v != "" {
			return []byte(v)
		}
		return []byte("")
	})

	// Restore escaped forms: $${VAR} -> ${VAR}
	data = bytes.ReplaceAll(data, []byte(escapedVarMarker+"{"), []byte("${"))
	return data
}
