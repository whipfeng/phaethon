//go:build windows

package main

import (
	"path/filepath"

	"github.com/UserExistsError/conpty"
)

// shouldInstallAssets mirrors the "on demand" rule: when ConPTY is available
// and the deployment directory has no winpty yet, leave it clean. If ConPTY is
// unavailable the assets are required; if winpty is already present, update it
// from the package to keep the fallback working.
func shouldInstallAssets(dir string, assets map[string][]byte) bool {
	if len(assets) == 0 {
		return false
	}
	if !conpty.IsConPtyAvailable() {
		return true
	}
	for name := range assets {
		if fileExists(filepath.Join(dir, name)) {
			return true
		}
	}
	return false
}
