package setup

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"gopkg.in/yaml.v3"

	"phaethon/config"
)

// profileMu protects concurrent LoadProfile/SaveProfile calls from multiple
// reverse-client goroutines and the admin API.
var profileMu sync.Mutex

const profilePath = ".phaethon/setup/profile.yaml"

// HasProfile returns true if a saved setup profile exists.
func HasProfile() bool {
	_, err := os.Stat(profilePath)
	return err == nil
}

// SaveProfile writes the RuleConfiguration to the setup profile file.
func SaveProfile(conf *config.RuleConfiguration) error {
	profileMu.Lock()
	defer profileMu.Unlock()

	dir := filepath.Dir(profilePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create dir fail: %w", err)
	}
	data, err := yaml.Marshal(conf)
	if err != nil {
		return fmt.Errorf("marshal fail: %w", err)
	}
	return os.WriteFile(profilePath, data, 0644)
}

// LoadProfile reads and parses the saved setup profile file.
func LoadProfile() (*config.RuleConfiguration, error) {
	profileMu.Lock()
	defer profileMu.Unlock()

	data, err := os.ReadFile(profilePath)
	if err != nil {
		return nil, fmt.Errorf("read fail: %w", err)
	}
	conf, err := config.LoadRawBytes(data)
	if err != nil {
		return nil, fmt.Errorf("parse fail: %w", err)
	}
	return conf, nil
}

// DeleteProfile removes the saved setup profile.
func DeleteProfile() error {
	os.Remove(profilePath) // ignore error
	return nil
}
