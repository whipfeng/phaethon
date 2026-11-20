package tun

import "testing"

// The health loop relies on InterfaceExists distinguishing "absent" from
// "could not determine". An absent interface must be (false, nil); only real
// enumeration failures may be reported as errors.
func TestInterfaceExistsAbsent(t *testing.T) {
	exists, err := InterfaceExists("phaethon-no-such-iface")
	if err != nil {
		t.Fatalf("absent interface returned an error, health loop would keep it: %v", err)
	}
	if exists {
		t.Fatalf("unexpected interface phaethon-no-such-iface")
	}
}

func TestInterfaceExistsEmptyName(t *testing.T) {
	exists, err := InterfaceExists("")
	if err != nil {
		t.Fatalf("empty name returned an error: %v", err)
	}
	if exists {
		t.Fatalf("empty name must not match an interface")
	}
}
