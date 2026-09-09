//go:build !windows

package p2p

// DetectBuildTag returns empty string on non-Windows platforms.
func DetectBuildTag() string {
	return ""
}
