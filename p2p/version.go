package p2p

import (
	"strconv"
	"strings"
)

// RunningVersion is set by main.go during initialization.
// It holds the current running version string.
var RunningVersion = "dev"

// runningVersion returns the current running version.
func runningVersion() string {
	return RunningVersion
}

// CompareVersions compares two version strings.
// Returns -1 if a < b, 0 if a == b, 1 if a > b.
// Supports versions like: v1.0.0, v1.0.0-5-gabc1234, v1.0.0-dirty, 78e9dfc, dev
func CompareVersions(a, b string) int {
	// Strip build metadata (+build)
	if idx := strings.Index(a, "+"); idx != -1 {
		a = a[:idx]
	}
	if idx := strings.Index(b, "+"); idx != -1 {
		b = b[:idx]
	}

	// Strip dirty suffix
	a = strings.TrimSuffix(a, "-dirty")
	b = strings.TrimSuffix(b, "-dirty")

	// Check if both are tags (start with 'v')
	aIsTag := strings.HasPrefix(a, "v")
	bIsTag := strings.HasPrefix(b, "v")

	// Tagged > untagged
	if aIsTag && !bIsTag {
		return 1
	}
	if !aIsTag && bIsTag {
		return -1
	}

	// Both untagged: string comparison
	if !aIsTag && !bIsTag {
		if a < b {
			return -1
		} else if a > b {
			return 1
		}
		return 0
	}

	// Both tags: parse and compare
	return compareTagVersions(a, b)
}

// compareTagVersions compares two tag versions like v1.0.0 vs v1.0.0-5-gabc1234
func compareTagVersions(a, b string) int {
	// Parse base version and commit count
	aBase, aCommits := parseTagVersion(a)
	bBase, bCommits := parseTagVersion(b)

	// Compare base versions
	if cmp := compareSemver(aBase, bBase); cmp != 0 {
		return cmp
	}

	// Same base version: more commits = newer
	if aCommits < bCommits {
		return -1
	} else if aCommits > bCommits {
		return 1
	}
	return 0
}

// parseTagVersion parses a tag version like "v1.0.0" or "v1.0.0-5-gabc1234"
// Returns base version and commit count (0 if not present)
func parseTagVersion(v string) (string, int) {
	// Remove 'v' prefix
	v = strings.TrimPrefix(v, "v")

	// Check for commit suffix: -5-gabc1234
	parts := strings.Split(v, "-")
	if len(parts) >= 3 {
		// v1.0.0-5-gabc1234 -> base=1.0.0, commits=5
		base := parts[0]
		for i := 1; i < len(parts)-2; i++ {
			base += "-" + parts[i]
		}
		commits, _ := strconv.Atoi(parts[len(parts)-2])
		return base, commits
	}

	// No commit suffix
	return v, 0
}

// compareSemver compares two semantic version strings like "1.0.0" vs "1.1.0"
func compareSemver(a, b string) int {
	aParts := strings.Split(a, ".")
	bParts := strings.Split(b, ".")

	maxLen := len(aParts)
	if len(bParts) > maxLen {
		maxLen = len(bParts)
	}

	for i := 0; i < maxLen; i++ {
		var aNum, bNum int
		if i < len(aParts) {
			aNum, _ = strconv.Atoi(aParts[i])
		}
		if i < len(bParts) {
			bNum, _ = strconv.Atoi(bParts[i])
		}

		if aNum < bNum {
			return -1
		} else if aNum > bNum {
			return 1
		}
	}

	return 0
}
