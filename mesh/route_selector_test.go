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
			decision := selector(test.dst)
			if decision.EgressNIC != 1 {
				t.Fatalf("EgressNIC = %d, want 1", decision.EgressNIC)
			}
			if decision.LocalStack != test.localStack {
				t.Fatalf("LocalStack = %t, want %t", decision.LocalStack, test.localStack)
			}
		})
	}
}
