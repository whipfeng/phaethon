package mesh

import (
	"net"
	"testing"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// fakePeerSender satisfies PeerSender for CreateLinkNIC without a live peer.
type fakePeerSender struct {
	sent [][]byte
}

func (f *fakePeerSender) Send(data []byte) error {
	f.sent = append(f.sent, append([]byte(nil), data...))
	return nil
}
func (f *fakePeerSender) SendGossip(data []byte)    {}
func (f *fakePeerSender) GossipFields() (bool, int) { return false, 0 }
func (f *fakePeerSender) LaneAckFields() (bool, bool, bool, []GossipLaneRx, int) {
	return false, false, false, nil, 0
}
func (f *fakePeerSender) GetNodeID() string               { return "qgw" }
func (f *fakePeerSender) GetProxyName() string            { return "" }
func (f *fakePeerSender) GetLocalSeq() uint16             { return 0 }
func (f *fakePeerSender) GetRemoteSeq() uint16            { return 0 }
func (f *fakePeerSender) GetFriendlyName() string         { return "qgw" }
func (f *fakePeerSender) HasProxy() bool                  { return false }
func (f *fakePeerSender) GetLinkNIC() *LinkNIC            { return nil }

// TestOrdinaryUDPViaLinkNICWrite reproduces the QG production error:
//
//	[TUN] ... WIN7_VPN (v2 dial: resolve spms.migu.cn via 100.209.0.3:
//	  resolve spms.migu.cn via 100.209.0.3: write: endpoint is in invalid state)
//
// An ordinary (non-RouteFunc) UDP socket — e.g. ResolveDomainViaDNS's query
// endpoint — bound with an unspecified local address and connected to a peer
// GIP (100.209.0.3:53) takes the RouteSelector LinkNIC egress branch in
// gvisor-fork stack.go findRouteInner: makeRoute is built with
// addressEndpoint=nil (LinkNICs have no bound addresses) and
// localAddressNIC=outgoingNIC. Route.WritePacket then rejects the route via
// isValidForOutgoing (route.go: localAddressEndpoint == nil), producing
// ErrInvalidEndpointState on every Write.
func TestOrdinaryUDPViaLinkNICWrite(t *testing.T) {
	subnet := &net.IPNet{IP: net.ParseIP("100.0.0.0").To4(), Mask: net.CIDRMask(16, 32)}
	peerSubnet := &net.IPNet{IP: net.ParseIP("100.209.0.0").To4(), Mask: net.CIDRMask(16, 32)}
	peerGIP := tcpip.AddrFrom4([4]byte{100, 209, 0, 3})

	n := NewNetstack()
	n.WriteLoopCloseCh = func() <-chan struct{} { return nil } // no TUN device in test
	if err := n.ConfigureMeshAddresses(subnet); err != nil {
		t.Fatalf("ConfigureMeshAddresses: %v", err)
	}
	if err := n.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer n.Stop()

	sender := &fakePeerSender{}
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

	// Mirror ResolveDomainViaDNS's exact socket sequence.
	ns := n.Stack()
	var wq waiter.Queue
	ep, terr := ns.NewEndpoint(udp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
	if terr != nil {
		t.Fatalf("NewEndpoint: %v", terr)
	}
	defer ep.Close()
	if terr := ep.Bind(tcpip.FullAddress{}); terr != nil {
		t.Fatalf("Bind: %v", terr)
	}
	if terr := ep.Connect(tcpip.FullAddress{Addr: peerGIP, Port: 53}); terr != nil {
		t.Fatalf("Connect: %v", terr)
	}

	query := BuildDNSQuery("spms.migu.cn", 0x1234)
	_, werr := ep.Write(&SlicePayload{Data: query}, tcpip.WriteOptions{})
	t.Logf("ordinary UDP Write via LinkNIC: err=%v", werr)
	if werr != nil {
		t.Fatalf("REPRODUCED: ordinary UDP socket via LinkNIC egress fails: %v", werr)
	}
	if len(sender.sent) == 0 {
		t.Fatalf("write succeeded but LinkNIC sender got no packet")
	}
}
