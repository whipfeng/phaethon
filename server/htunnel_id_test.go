package server

import "testing"

// TestHTChannelIDMonotonic verifies server-issued channel IDs carry a
// boot-unique epoch and increase monotonically within the process, so a stale
// client ID from a previous boot can never match a fresh channel.
func TestHTChannelIDMonotonic(t *testing.T) {
	s := &HTunnelServer{}
	a := nextHTChannelID(s)
	b := nextHTChannelID(s)
	if a >= b {
		t.Fatalf("channel IDs not increasing: %d then %d", a, b)
	}
	if a <= htChannelEpoch {
		t.Fatalf("channel ID %d does not include epoch %d", a, htChannelEpoch)
	}
}
