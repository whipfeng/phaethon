package frame

import "testing"

func TestLaneLowatDefaultAndLabOverride(t *testing.T) {
	t.Setenv("PHAETHON_LAB", "")
	t.Setenv("PHAETHON_P2P_LOWAT", "131072")
	if got := laneLowatBytes(); got != laneNotSentLowat {
		t.Fatalf("production lowat = %d, want %d (env must be ignored)", got, laneNotSentLowat)
	}
	if laneNotSentLowat < 64<<10 {
		t.Fatalf("platform floor = %d, want >= 64KB", laneNotSentLowat)
	}

	t.Setenv("PHAETHON_LAB", "1")
	if got := laneLowatBytes(); got != 131072 {
		t.Fatalf("lab lowat = %d, want 131072", got)
	}
}
