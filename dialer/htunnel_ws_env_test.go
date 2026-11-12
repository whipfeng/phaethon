package dialer

import "testing"

// TestWSEnabledForDefault verifies WebSocket carrier selection: unset means
// try-WS-first (the active side chooses the carrier for the whole P2P session),
// explicit off forces HTTP, and a name list is an allowlist.
func TestWSEnabledForDefault(t *testing.T) {
	cases := []struct {
		name  string
		env   string
		proxy string
		want  bool
	}{
		{"default_on", "", "MGMS_HT", true},
		{"explicit_off", "0", "MGMS_HT", false},
		{"off_word", "off", "MGMS_HT", false},
		{"explicit_on", "1", "MGMS_HT", true},
		{"auto", "auto", "MGMS_HT", true},
		{"all", "all", "MGMS_HT", true},
		{"allowlist_hit", "MB_HT,MGMS_HT", "MGMS_HT", true},
		{"allowlist_miss", "MB_HT", "MGMS_HT", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PHAETHON_HT_WS", tc.env)
			if got := wsEnabledFor(tc.proxy); got != tc.want {
				t.Fatalf("wsEnabledFor(%q) with env %q = %v, want %v", tc.proxy, tc.env, got, tc.want)
			}
		})
	}
}
