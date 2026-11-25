package mesh

import (
	"net"
	"testing"

	"gvisor.dev/gvisor/pkg/tcpip"
)

func TestRouteSelectorLocalStack(t *testing.T) {
	_, subnet, err := net.ParseCIDR("100.64.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	vip := tcpip.AddrFrom4([4]byte{100, 64, 0, 1})
	gip := tcpip.AddrFrom4([4]byte{100, 64, 0, 3})
	fakeIP := tcpip.AddrFrom4([4]byte{100, 64, 0, 42})
	selector := NewRouteSelector(&RouteSelectorConfig{
		LocalSubnet: subnet,
		LocalVIP:    vip,
		LocalGIP:    gip,
		IsFakeIP: func(ip net.IP) bool {
			return ip.Equal(fakeIP.AsSlice())
		},
	})

	tests := []struct {
		name       string
		dst        tcpip.Address
		localStack bool
	}{
		{name: "VIP is conntrack transit", dst: vip, localStack: false},
		{name: "GIP is local service", dst: gip, localStack: true},
		{name: "fake IP is local proxy entry", dst: fakeIP, localStack: true},
		{name: "unrouted destination is local proxy entry", dst: tcpip.AddrFrom4([4]byte{192, 0, 2, 1}), localStack: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := selector(tcpip.Address{}, test.dst)
			if decision.EgressNIC != 1 {
				t.Fatalf("EgressNIC = %d, want 1", decision.EgressNIC)
			}
			if decision.LocalStack != test.localStack {
				t.Fatalf("LocalStack = %t, want %t", decision.LocalStack, test.localStack)
			}
		})
	}
}

// TestRouteSelectorPropagatesSource pins down that the selector hands the
// packet source to SelectRoute unchanged. The receive-path probe
// (RouteSelectorLocalStack) and the forwarding path must share one decision
// key, otherwise a source-aware owner gets cached under a source-less key.
func TestRouteSelectorPropagatesSource(t *testing.T) {
	_, localSubnet, err := net.ParseCIDR("100.64.1.0/24")
	if err != nil {
		t.Fatal(err)
	}
	var gotSrc, gotDst net.IP
	calls := 0
	selector := NewRouteSelector(&RouteSelectorConfig{
		LocalSubnet: localSubnet,
		SelectRoute: func(src, dst net.IP) (SelectRouteResult, bool) {
			calls++
			gotSrc, gotDst = src, dst
			return SelectRouteResult{
				LinkNIC:   2,
				NeedIPIP:  true,
				EgressEIP: net.IP{100, 64, 1, 4},
			}, true
		},
	})

	src := tcpip.AddrFrom4([4]byte{192, 168, 1, 5})
	dst := tcpip.AddrFrom4([4]byte{10, 161, 88, 9})
	selector(src, dst)
	if calls != 1 {
		t.Fatalf("SelectRoute calls = %d, want 1", calls)
	}
	if !gotSrc.Equal(net.IP{192, 168, 1, 5}) {
		t.Fatalf("SelectRoute src = %v, want 192.168.1.5", gotSrc)
	}
	if !gotDst.Equal(net.IP{10, 161, 88, 9}) {
		t.Fatalf("SelectRoute dst = %v, want 10.161.88.9", gotDst)
	}

	selector(tcpip.Address{}, dst)
	if calls != 2 {
		t.Fatalf("SelectRoute calls = %d, want 2", calls)
	}
	if gotSrc != nil {
		t.Fatalf("SelectRoute src for zero Address = %v, want nil", gotSrc)
	}
}
