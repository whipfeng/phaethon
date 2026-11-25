//go:build !windows

package main

// Unix packages never carry PTY assets; skip the whole asset path there.
func shouldInstallAssets(dir string, assets map[string][]byte) bool {
	return false
}
