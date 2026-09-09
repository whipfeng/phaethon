package p2p

import "testing"

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		// Equal
		{"v1.0.0", "v1.0.0", 0},
		{"dev", "dev", 0},
		{"78e9dfc", "78e9dfc", 0},

		// Tag comparison
		{"v1.0.0", "v1.1.0", -1},
		{"v1.1.0", "v1.0.0", 1},
		{"v2.0.0", "v1.9.9", 1},

		// Tag + commits
		{"v1.0.0", "v1.0.0-5-gabc1234", -1},
		{"v1.0.0-5-gabc1234", "v1.0.0", 1},
		{"v1.0.0-3-gabc1234", "v1.0.0-5-gabc1234", -1},

		// Dirty suffix
		{"v1.0.0-dirty", "v1.0.0", 0},
		{"v1.0.0-5-gabc1234-dirty", "v1.0.0-5-gabc1234", 0},

		// Tagged > untagged
		{"v1.0.0", "78e9dfc", 1},
		{"78e9dfc", "v1.0.0", -1},
		{"v0.0.1", "78e9dfc", 1},

		// Untagged vs untagged (string comparison)
		{"dev", "78e9dfc", -1},
		{"abc", "def", -1},
	}

	for _, tt := range tests {
		got := compareVersions(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}
