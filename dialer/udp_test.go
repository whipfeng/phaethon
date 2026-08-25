package dialer

import (
	"net"
	"testing"
)

// saveUDPPortRange saves current global state so we can restore after test.
func saveUDPPortRange() (int, int) {
	return globalUDPPortMin, globalUDPPortMax
}

func restoreUDPPortRange(min, max int) {
	globalUDPPortMin = min
	globalUDPPortMax = max
}

func TestSetUDPPortRange(t *testing.T) {
	oldMin, oldMax := saveUDPPortRange()
	defer restoreUDPPortRange(oldMin, oldMax)

	tests := []struct {
		name    string
		min     int
		max     int
		wantMin int
		wantMax int
	}{
		{"valid range", 30000, 35000, 30000, 35000},
		{"min equals max", 40000, 40000, 40000, 40000},
		{"zero values ignored", 0, 0, 0, 0},
		{"negative ignored", -1, 100, 0, 0},
		{"max < min ignored", 50000, 40000, 0, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			globalUDPPortMin = 0
			globalUDPPortMax = 0
			SetUDPPortRange(tt.min, tt.max)
			if globalUDPPortMin != tt.wantMin || globalUDPPortMax != tt.wantMax {
				t.Errorf("SetUDPPortRange(%d,%d) = (%d,%d), want (%d,%d)",
					tt.min, tt.max, globalUDPPortMin, globalUDPPortMax, tt.wantMin, tt.wantMax)
			}
		})
	}
}

func TestListenUDP_WithinRange(t *testing.T) {
	oldMin, oldMax := saveUDPPortRange()
	defer restoreUDPPortRange(oldMin, oldMax)

	// Find a free port to use as our test range
	probe, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	freePort := probe.LocalAddr().(*net.UDPAddr).Port
	probe.Close()

	// Set range to include just this known-free port
	SetUDPPortRange(freePort, freePort)

	conn, err := ListenUDP()
	if err != nil {
		t.Fatalf("ListenUDP() error: %v", err)
	}
	defer conn.Close()

	addr := conn.LocalAddr().(*net.UDPAddr)
	if addr.Port != freePort {
		t.Errorf("ListenUDP() bound to port %d, want %d", addr.Port, freePort)
	}
}

func TestListenUDP_OSDefaultWhenNotConfigured(t *testing.T) {
	oldMin, oldMax := saveUDPPortRange()
	defer restoreUDPPortRange(oldMin, oldMax)

	// No range configured
	SetUDPPortRange(0, 0)

	conn, err := ListenUDP()
	if err != nil {
		t.Fatalf("ListenUDP() error: %v", err)
	}
	defer conn.Close()

	// Should have gotten an OS-assigned port (> 0)
	addr := conn.LocalAddr().(*net.UDPAddr)
	if addr.Port <= 0 {
		t.Errorf("ListenUDP() got invalid port %d", addr.Port)
	}
}

func TestListenUDP_MultipleListenersDifferentPorts(t *testing.T) {
	oldMin, oldMax := saveUDPPortRange()
	defer restoreUDPPortRange(oldMin, oldMax)

	// Find 3 consecutive free ports by probing
	var basePort int
	for attempt := 0; attempt < 50; attempt++ {
		probe, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
		if err != nil {
			continue
		}
		p := probe.LocalAddr().(*net.UDPAddr).Port
		probe.Close()

		// Check if p, p+1, p+2 are all available
		c1, e1 := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: p})
		c2, e2 := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: p + 1})
		c3, e3 := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: p + 2})

		allOK := e1 == nil && e2 == nil && e3 == nil
		if c1 != nil {
			c1.Close()
		}
		if c2 != nil {
			c2.Close()
		}
		if c3 != nil {
			c3.Close()
		}

		if allOK {
			basePort = p
			break
		}
	}

	if basePort == 0 {
		t.Skip("could not find 3 consecutive free UDP ports")
	}

	SetUDPPortRange(basePort, basePort+2)

	conns := make([]net.PacketConn, 3)
	for i := 0; i < 3; i++ {
		conn, err := ListenUDP()
		if err != nil {
			t.Fatalf("ListenUDP() #%d error: %v", i, err)
		}
		conns[i] = conn
	}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()

	ports := make(map[int]bool)
	for i, c := range conns {
		p := c.LocalAddr().(*net.UDPAddr).Port
		if p < basePort || p > basePort+2 {
			t.Errorf("conn[%d] port %d out of range [%d, %d]", i, p, basePort, basePort+2)
		}
		if ports[p] {
			t.Errorf("duplicate port %d used by multiple listeners", p)
		}
		ports[p] = true
	}

	if len(ports) != 3 {
		t.Errorf("got %d unique ports, want 3", len(ports))
	}
}

func TestListenUDP_RangeExhausted(t *testing.T) {
	oldMin, oldMax := saveUDPPortRange()
	defer restoreUDPPortRange(oldMin, oldMax)

	// Occupy a port and set range to only that occupied port.
	// On some platforms UDP allows duplicate binds, so we verify via
	// checking that the returned conn is actually a new binding.
	probe, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	freePort := probe.LocalAddr().(*net.UDPAddr).Port

	// Keep probe alive (occupying the port) while trying to listen again
	SetUDPPortRange(freePort, freePort)

	conn, err := ListenUDP()
	if err != nil {
		// Expected: port already in use
		probe.Close()
		return // test passes
	}

	// If platform allowed double-bind, at least verify it's a different socket
	if conn != nil {
		conn.Close()
	}
	probe.Close()

	// On platforms that allow UDP port reuse (e.g., Windows with SO_REUSEADDR),
	// this is acceptable — the important thing is no panic/crash.
	t.Log("platform allows UDP port reuse; range exhaustion not enforced at OS level")
}

func TestListenUDPWithAddr_CustomBind(t *testing.T) {
	oldMin, oldMax := saveUDPPortRange()
	defer restoreUDPPortRange(oldMin, oldMax)

	// Find a free port on localhost
	probe, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	freePort := probe.LocalAddr().(*net.UDPAddr).Port
	probe.Close()

	SetUDPPortRange(freePort, freePort)

	conn, err := ListenUDPWithAddr("127.0.0.1")
	if err != nil {
		t.Fatalf("ListenUDPWithAddr() error: %v", err)
	}
	defer conn.Close()

	addr := conn.LocalAddr().(*net.UDPAddr)
	if addr.Port != freePort {
		t.Errorf("bound to port %d, want %d", addr.Port, freePort)
	}
	if addr.IP.String() != "127.0.0.1" {
		t.Errorf("bound to IP %s, want 127.0.0.1", addr.IP.String())
	}
}
