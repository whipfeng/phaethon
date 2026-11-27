package config

import "testing"

func boolPtr(v bool) *bool { return &v }

func TestIsP2P_HTunnelAlwaysOn(t *testing.T) {
	p := &Proxy{Type: "H_TUNNEL", P2P: boolPtr(false)}
	if !p.IsP2P() {
		t.Fatal("h_tunnel must always report P2P enabled")
	}
}

func TestIsP2P_RespectsExplicitFlagForOtherTypes(t *testing.T) {
	if (&Proxy{Type: "TROJAN", P2P: boolPtr(false)}).IsP2P() {
		t.Fatal("trojan with p2p=false must report disabled")
	}
	if !(&Proxy{Type: "TROJAN"}).IsP2P() {
		t.Fatal("trojan without an explicit flag defaults to enabled")
	}
}

func TestSameP2PSession_IgnoresRuntimeOnlyFields(t *testing.T) {
	a := &Proxy{Name: "p", Type: "TROJAN", Server: "1.2.3.4", Port: 443, Password: "x"}
	b := &Proxy{Name: "p", Type: "TROJAN", Server: "1.2.3.4", Port: 443, Password: "x"}
	b.Next = &Proxy{Name: "next", Type: "SOCKS5"}
	b.UpRateLimiter = NewRateLimiter("up", 1024)
	b.DownRateLimiter = NewRateLimiter("down", 1024)
	if !a.SameP2PSession(b) {
		t.Fatal("runtime-only fields must not force a session restart")
	}
}

func TestSameP2PSession_DetectsConfigChange(t *testing.T) {
	a := &Proxy{Name: "p", Type: "TROJAN", Server: "1.2.3.4", Port: 443, Password: "x"}
	b := &Proxy{Name: "p", Type: "TROJAN", Server: "1.2.3.4", Port: 443, Password: "y"}
	if a.SameP2PSession(b) {
		t.Fatal("password change must be detected")
	}
	b.Password = "x"
	b.P2PUDP = boolPtr(true)
	if a.SameP2PSession(b) {
		t.Fatal("p2p-udp change must be detected")
	}
}
