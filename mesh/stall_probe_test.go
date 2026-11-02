package mesh

import (
	"net"
	"path/filepath"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
)

// newProbeTestNetstack builds the same netstack + LinkNIC topology as the
// reproduction test (local subnet 100.0.0.0/16, one peer "qgw" at
// 100.209.0.0/16). RouteSelector sends peer-subnet traffic out the LinkNIC.
func newProbeTestNetstack(t *testing.T, sender PeerSender) (*Netstack, *LinkNIC) {
	t.Helper()
	subnet := &net.IPNet{IP: net.ParseIP("100.0.0.0").To4(), Mask: net.CIDRMask(16, 32)}
	peerSubnet := &net.IPNet{IP: net.ParseIP("100.209.0.0").To4(), Mask: net.CIDRMask(16, 32)}

	n := NewNetstack()
	n.WriteLoopCloseCh = func() <-chan struct{} { return nil }
	if err := n.ConfigureMeshAddresses(subnet); err != nil {
		t.Fatalf("ConfigureMeshAddresses: %v", err)
	}
	pool := NewFakeIPPoolWithSubnet(subnet, 9)
	n.SetDNSHijacker(NewDNSHijacker(nil, pool, tcpip.Address{}, tcpip.Address{}), pool)
	if err := n.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { n.Stop() })
	if err := n.BindNetstack(); err != nil {
		t.Fatalf("BindNetstack: %v", err)
	}
	if err := n.StartDNSHijacker(); err != nil {
		t.Fatalf("StartDNSHijacker: %v", err)
	}
	// The hijacker's workers are registered on the netstack WaitGroup but
	// Netstack.Stop does not stop the hijacker — production stops it via
	// mesh shutdown. Stop it here (cleanup is LIFO) or cleanup hangs in
	// wg.Wait. t.Cleanup below runs BEFORE the n.Stop cleanup above.
	t.Cleanup(func() { n.GetDNSHijacker().Stop() })

	linkNIC, err := n.CreateLinkNIC(sender, "qgw", peerSubnet)
	if err != nil {
		t.Fatalf("CreateLinkNIC: %v", err)
	}
	n.Stack().SetRouteSelector(NewRouteSelector(&RouteSelectorConfig{
		LocalSubnet: subnet,
		LocalVIP:    tcpip.AddrFrom4Slice(CalculateVIP(subnet)),
		LocalGIP:    n.dnsAddr,
		SelectRoute: func(dst net.IP) (SelectRouteResult, bool) {
			if peerSubnet.Contains(dst) {
				return SelectRouteResult{LinkNIC: linkNIC.NICID()}, true
			}
			return SelectRouteResult{}, false
		},
	}))
	return n, linkNIC
}

// TestProbeMeshPeerDNS_Alive exercises the probe's socket/waiter mechanics
// against the REAL local DNS hijacker: probing the own GIP stays in-stack
// (branch 1 LocalStack), the hijacker answers, the probe returns alive. The
// LinkNIC egress half of the path is covered by
// TestOrdinaryUDPViaLinkNICWrite; the full mesh round trip is validated in
// production via POST /debug/stall-probe (docs/plans/stall-monitor-precise-dump-ladder.md).
func TestProbeMeshPeerDNS_Alive(t *testing.T) {
	n, _ := newProbeTestNetstack(t, &fakePeerSender{})
	addr := n.DNSAddr()
	ownGIP := net.IP(addr.AsSlice())

	if err := n.ProbeMeshPeerDNS(ownGIP, 2*time.Second); err != nil {
		t.Fatalf("probe to local hijacker: %v", err)
	}
}

func TestProbeMeshPeerDNS_Dead(t *testing.T) {
	sender := &fakePeerSender{} // captures but never answers
	n, _ := newProbeTestNetstack(t, sender)
	peerGIP := net.ParseIP("100.209.0.3").To4()

	err := n.ProbeMeshPeerDNS(peerGIP, 300*time.Millisecond)
	if err == nil {
		t.Fatalf("probe to dead peer succeeded, want timeout error")
	}
	if len(sender.sent) == 0 {
		t.Fatalf("probe produced no egress packet")
	}
	t.Logf("dead peer probe err: %v", err)
}

func TestClassifyDisposition(t *testing.T) {
	healthy := []stallPeerSnap{{Node: "qg", ConsecLost: 0, LossRate: 0.01}}
	deadByLoss := []stallPeerSnap{{Node: "qg", ConsecLost: 7}}
	deadByRate := []stallPeerSnap{{Node: "qg", LossRate: 0.5}}

	cases := []struct {
		name    string
		rule    int
		peers   []stallPeerSnap
		want    string
	}{
		{"no peers", 0, nil, "ambiguous"},
		{"dead by consecLost", 0, deadByLoss, "dead-link-suspect"},
		{"dead by lossRate", 1, deadByRate, "dead-link-suspect"},
		{"rule2 healthy", 2, healthy, "host-silent-suspect"},
		{"rule0 healthy", 0, healthy, "ping-healthy"},
		{"dead wins over host-silent", 2, append(deadByLoss, healthy...), "dead-link-suspect"},
	}
	for _, tc := range cases {
		if got := classifyDisposition(tc.rule, tc.peers); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// fireRule2 drives the sample sequence that arms rule 2 (B rising, A frozen
// after flowing), then calls fire() directly with fake probes.
func fireRule2(t *testing.T, sm *StallMonitor) string {
	t.Helper()
	base := time.Now()
	feed(sm, base, 0, 0, 0, 0)
	for sec := 10; sec <= 50; sec += 10 {
		feed(sm, base, sec, uint64(sec*10), uint64(sec), 3)
	}
	feed(sm, base, 60, 500, 60, 3)
	if f := feed(sm, base, 110, 500, 110, 3); len(f) != 1 || f[0] != 2 {
		t.Fatalf("setup: expected rule 2 fire, got %v", f)
	}
	sm.fire(2, sample{t: base.Add(110 * time.Second), a: 500, b: 110, dVIP: 3}, base.Add(110*time.Second))
	matches, _ := filepath.Glob(filepath.Join(sm.logsDir, "goroutine-dump-*.log"))
	if len(matches) == 0 {
		return ""
	}
	return matches[0]
}

func TestFireRule2SkipsDumpWhenHostLoopAlive(t *testing.T) {
	sm := NewStallMonitor(&MeshManager{}, nil, nil, "test", t.TempDir())
	sm.ownGIP = func() net.IP { return net.ParseIP("100.0.0.3") }
	sm.probeHost = func(net.IP, time.Duration) error { return nil }

	if path := fireRule2(t, sm); path != "" {
		t.Fatalf("host loop alive but dump written: %s", path)
	}
}

func TestFireRule2WritesDumpWhenHostLoopDead(t *testing.T) {
	sm := NewStallMonitor(&MeshManager{}, nil, nil, "test", t.TempDir())
	sm.ownGIP = func() net.IP { return net.ParseIP("100.0.0.3") }
	sm.probeHost = func(net.IP, time.Duration) error { return errProbeTimeout }

	if path := fireRule2(t, sm); path == "" {
		t.Fatalf("host loop dead but no dump written")
	}
}

func TestFireRule0AlwaysDumps(t *testing.T) {
	sm := NewStallMonitor(&MeshManager{}, nil, nil, "test", t.TempDir())

	base := time.Now()
	feed(sm, base, 0, 0, 0, 0)
	for sec := 10; sec <= 50; sec += 10 {
		feed(sm, base, sec, uint64(sec), 0, uint64(sec*10))
	}
	feed(sm, base, 60, 60, 0, 500)
	if f := feed(sm, base, 110, 110, 0, 500); len(f) != 1 || f[0] != 0 {
		t.Fatalf("setup: expected rule 0 fire, got %v", f)
	}
	sm.fire(0, sample{t: base.Add(110 * time.Second), a: 110, dVIP: 500}, base.Add(110*time.Second))

	matches, _ := filepath.Glob(filepath.Join(sm.logsDir, "goroutine-dump-*.log"))
	if len(matches) == 0 {
		t.Fatalf("rule 0 must always dump (mesh leg evidence), none written")
	}
}

var errProbeTimeout = errorString("probe timeout")

type errorString string

func (e errorString) Error() string { return string(e) }

func TestProbeHostViaOwnGIPTimeout(t *testing.T) {
	// 192.0.2.0/24 is TEST-NET: DialUDP succeeds, nothing answers, the read
	// deadline fires. Asserts the host leg fails fast and never panics.
	start := time.Now()
	err := probeHostViaOwnGIP(net.ParseIP("192.0.2.1").To4(), 300*time.Millisecond)
	if err == nil {
		t.Fatalf("probe to TEST-NET succeeded, want timeout")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("probe took too long: %v", elapsed)
	}
}
