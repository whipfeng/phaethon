package config

import "testing"

func TestMeshConfigEntryIDs(t *testing.T) {
	m := &MeshConfig{
		Advertise:      []string{"10.0.0.0/8", "192.168.1.0/24"},
		DomainSuffixes: []string{"a.example", "b.example"},
	}
	if !m.EnsureEntryIDs() {
		t.Fatal("EnsureEntryIDs first call should report a change")
	}
	if len(m.AdvertiseIDs) != 2 || len(m.DomainSuffixIDs) != 2 {
		t.Fatalf("id lengths = %d/%d, want 2/2", len(m.AdvertiseIDs), len(m.DomainSuffixIDs))
	}
	if m.AdvertiseIDs[0] == 0 || m.AdvertiseIDs[0] == m.AdvertiseIDs[1] {
		t.Fatalf("advertise IDs not unique/non-zero: %v", m.AdvertiseIDs)
	}
	if got := m.AdvertiseIndexByID(m.AdvertiseIDs[1]); got != 1 {
		t.Fatalf("AdvertiseIndexByID = %d, want 1", got)
	}
	if got := m.AdvertiseIndexByID(1 << 40); got != -1 {
		t.Fatalf("AdvertiseIndexByID(unknown) = %d, want -1", got)
	}
	if got := m.DomainSuffixIndexByID(m.DomainSuffixIDs[0]); got != 0 {
		t.Fatalf("DomainSuffixIndexByID = %d, want 0", got)
	}
	// Second call must be a no-op.
	if m.EnsureEntryIDs() {
		t.Fatal("EnsureEntryIDs second call reported a change")
	}
	// Shrinking the value list trims the IDs.
	m.Advertise = m.Advertise[:1]
	if !m.EnsureEntryIDs() {
		t.Fatal("EnsureEntryIDs should report the truncation")
	}
	if len(m.AdvertiseIDs) != 1 {
		t.Fatalf("advertise IDs length = %d, want 1", len(m.AdvertiseIDs))
	}
}

func TestMeshConfigEntryIDsDuplicateValues(t *testing.T) {
	m := &MeshConfig{Advertise: []string{"10.0.0.0/8", "10.0.0.0/8"}}
	m.EnsureEntryIDs()
	if m.AdvertiseIDs[0] == m.AdvertiseIDs[1] {
		t.Fatalf("duplicate values must get distinct IDs: %v", m.AdvertiseIDs)
	}
}
