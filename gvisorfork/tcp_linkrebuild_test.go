package gvisorfork

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"

	"phaethon/util"
)

// TestSackedOutNeverNegative samples the sender's SackedOut during
// transfers with reordering, loss and an RTO black hole.
func TestSackedOutNeverNegative(t *testing.T) {
	cases := map[string]policy{
		"reorder": func(n int) (time.Duration, bool) {
			if n%40 == 0 {
				return 8 * time.Millisecond, false
			}
			return 0, false
		},
		"loss": func(n int) (time.Duration, bool) { return 0, n%300 == 0 },
		"reorder+loss": func(n int) (time.Duration, bool) {
			if n%40 == 0 {
				return 8 * time.Millisecond, false
			}
			return 0, n%170 == 0
		},
	}
	var start time.Time
	var once sync.Once
	cases["rto"] = func(n int) (time.Duration, bool) {
		once.Do(func() { start = time.Now() })
		el := time.Since(start)
		if n%40 == 0 {
			return 8 * time.Millisecond, false
		}
		return 0, el > 50*time.Millisecond && el < 1500*time.Millisecond
	}
	for name, pol := range cases {
		t.Run(name, func(t *testing.T) {
			done := make(chan struct{})
			var samples, minSacked, leaks atomic.Int64
			go func() {
				for {
					select {
					case <-done:
						return
					case <-time.After(500 * time.Microsecond):
					}
					for _, f := range tcp.DumpTCPFlowDebug(func(_, remote tcpip.FullAddress) bool { return remote.Port == 8080 }) {
						if !f.HasSender {
							continue
						}
						samples.Add(1)
						if int64(f.SackedOut) < minSacked.Load() {
							minSacked.Store(int64(f.SackedOut))
						}
						if f.SndUna == f.SndNxt && f.SackedOut != 0 {
							leaks.Add(1)
						}
					}
				}
			}()
			s := transfer(t, 8<<20, 10*time.Millisecond, pol)
			close(done)
			logStats(t, s)
			t.Logf("samples=%d minSackedOut=%d leaks=%d", samples.Load(), minSacked.Load(), leaks.Load())
			if samples.Load() == 0 {
				t.Fatalf("no sender samples taken")
			}
			if minSacked.Load() < 0 {
				t.Fatalf("SackedOut went negative: %d", minSacked.Load())
			}
			if leaks.Load() != 0 {
				t.Fatalf("SackedOut non-zero with nothing in flight in %d samples", leaks.Load())
			}
		})
	}
}

// TestRerouteAfterNICRemoval removes the egress NIC mid-transfer (as a mesh
// link rebuild does) and expects the established connection to move to the
// replacement NIC and finish.
func TestRerouteAfterNICRemoval(t *testing.T) {
	util.SetModuleLogLevel("gvisor", util.LogLevelInfo)
	defer util.SetModuleLogLevel("gvisor", util.LogLevelError)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addrA := tcpip.AddrFrom4([4]byte{10, 0, 0, 1})
	addrB := tcpip.AddrFrom4([4]byte{10, 0, 0, 2})

	sa := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol},
	})
	defer sa.Close()

	sb, eb := newStack(t, addrB)
	defer sb.Close()

	var cur atomic.Pointer[channel.Endpoint]
	addLink := func(id tcpip.NICID) *channel.Endpoint {
		ep := channel.New(4096, 1500, "")
		if err := sa.CreateNIC(id, ep); err != nil {
			t.Fatalf("CreateNIC %d: %v", id, err)
		}
		if err := sa.AddProtocolAddress(id, tcpip.ProtocolAddress{
			Protocol:          ipv4.ProtocolNumber,
			AddressWithPrefix: tcpip.AddressWithPrefix{Address: addrA, PrefixLen: 24},
		}, stack.AddressProperties{}); err != nil {
			t.Fatalf("AddProtocolAddress: %v", err)
		}
		sa.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: id}})
		pump(ctx, ep, eb, 5*time.Millisecond, nil)
		cur.Store(ep)
		return ep
	}
	addLink(2)

	// B → whichever A link NIC is current.
	go func() {
		for {
			pkt := eb.ReadContext(ctx)
			if pkt == nil {
				return
			}
			data := append([]byte(nil), pkt.ToView().AsSlice()...)
			pkt.DecRef()
			go func(ep *channel.Endpoint) {
				time.Sleep(5 * time.Millisecond)
				ep.InjectInbound(ipv4.ProtocolNumber, stack.NewPacketBuffer(stack.PacketBufferOptions{
					Payload: buffer.MakeWithData(data),
				}))
			}(cur.Load())
		}
	}()

	ln, err := gonet.ListenTCP(sb, tcpip.FullAddress{NIC: 1, Addr: addrB, Port: 8080}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	const size = 64 << 20
	got := make(chan int64, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			got <- -1
			return
		}
		defer c.Close()
		n, _ := io.Copy(io.Discard, c)
		got <- n
	}()

	dctx, dcancel := context.WithTimeout(ctx, 5*time.Second)
	defer dcancel()
	c, err := gonet.DialTCPWithBind(dctx, sa, tcpip.FullAddress{Addr: addrA}, tcpip.FullAddress{Addr: addrB, Port: 8080}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	writeErr := make(chan error, 1)
	go func() {
		buf := make([]byte, 64<<10)
		for sent := 0; sent < size; sent += len(buf) {
			if _, err := c.Write(buf); err != nil {
				writeErr <- err
				return
			}
		}
		writeErr <- c.Close()
	}()

	time.Sleep(100 * time.Millisecond)
	if err := sa.RemoveNIC(2); err != nil {
		t.Fatalf("RemoveNIC: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	addLink(3)
	go func() {
		for i := 1; i <= 8; i++ {
			select {
			case <-ctx.Done():
				return
			case <-time.After(250 * time.Millisecond):
			}
			for _, f := range tcp.DumpTCPFlowDebug(func(_, remote tcpip.FullAddress) bool { return remote.Port == 8080 }) {
				t.Logf("t+%dms una=%d nxt=%d cwnd=%d ssth=%d out=%d sacked=%d state=%s rto=%d resend=%s probe=%s lastSend=%dms timeouts=%d", i*250, f.SndUna, f.SndNxt, f.Cwnd, f.Ssthresh, f.Outstanding, f.SackedOut, f.CongestState, f.RTOus, f.ResendTimer, f.ProbeTimer, f.LastSendAgoMs, sa.Stats().TCP.Timeouts.Value())
			}
		}
	}()

	select {
	case n := <-got:
		if n != size {
			t.Fatalf("received %d bytes, want %d", n, size)
		}
	case <-time.After(30 * time.Second):
		for _, f := range tcp.DumpTCPFlowDebug(func(_, remote tcpip.FullAddress) bool { return remote.Port == 8080 }) {
			t.Logf("flow: %+v", f)
		}
		t.Fatalf("transfer stalled after NIC rebuild")
	}
	if err := <-writeErr; err != nil {
		t.Fatalf("write: %v", err)
	}
	// Sends must have failed on the dead route, proving the NIC went away
	// mid-transfer rather than after it finished.
	if n := sa.Stats().TCP.SegmentSendErrors.Value(); n == 0 {
		t.Fatalf("no send failures: NIC removal did not hit the live connection")
	} else {
		t.Logf("send failures during rebuild: %d", n)
	}
}
