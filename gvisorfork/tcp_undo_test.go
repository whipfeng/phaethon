package gvisorfork

import (
	"container/heap"
	"context"
	"io"
	"sync"
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
)

// verdict for one A→B data packet: extra delay (reorder), or drop.
type policy func(n int) (extra time.Duration, drop bool)

type timedPkt struct {
	at   time.Time
	seq  int
	data []byte
}

type pktHeap []timedPkt

func (h pktHeap) Len() int { return len(h) }
func (h pktHeap) Less(i, j int) bool {
	if h[i].at.Equal(h[j].at) {
		return h[i].seq < h[j].seq
	}
	return h[i].at.Before(h[j].at)
}
func (h pktHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *pktHeap) Push(x any)   { *h = append(*h, x.(timedPkt)) }
func (h *pktHeap) Pop() any     { o := *h; x := o[len(o)-1]; *h = o[:len(o)-1]; return x }

// pump moves packets from src to dst with a fixed one-way delay; pol (may be
// nil) is applied to TCP segments carrying payload.
func pump(ctx context.Context, src, dst *channel.Endpoint, delay time.Duration, pol policy) {
	var mu sync.Mutex
	h := &pktHeap{}
	wake := make(chan struct{}, 1)
	go func() {
		n, seq := 0, 0
		for {
			pkt := src.ReadContext(ctx)
			if pkt == nil {
				return
			}
			b := pkt.ToView().AsSlice()
			data := append([]byte(nil), b...)
			pkt.DecRef()
			at := time.Now().Add(delay)
			if pol != nil {
				ip := header.IPv4(data)
				if ip.TransportProtocol() == header.TCPProtocolNumber {
					tcpHdr := header.TCP(ip.Payload())
					if len(tcpHdr.Payload()) > 0 {
						n++
						extra, drop := pol(n)
						if drop {
							continue
						}
						at = at.Add(extra)
					}
				}
			}
			seq++
			mu.Lock()
			heap.Push(h, timedPkt{at: at, seq: seq, data: data})
			mu.Unlock()
			select {
			case wake <- struct{}{}:
			default:
			}
		}
	}()
	go func() {
		for {
			mu.Lock()
			var next *timedPkt
			if h.Len() > 0 {
				p := (*h)[0]
				next = &p
			}
			mu.Unlock()
			wait := time.Hour
			if next != nil {
				wait = time.Until(next.at)
			}
			if wait <= 0 {
				mu.Lock()
				p := heap.Pop(h).(timedPkt)
				mu.Unlock()
				dst.InjectInbound(ipv4.ProtocolNumber, stack.NewPacketBuffer(stack.PacketBufferOptions{
					Payload: buffer.MakeWithData(p.data),
				}))
				continue
			}
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-wake:
			case <-t.C:
			}
			t.Stop()
		}
	}()
}

func newStack(t *testing.T, addr tcpip.Address) (*stack.Stack, *channel.Endpoint) {
	t.Helper()
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol},
	})
	sack := tcpip.TCPSACKEnabled(true)
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &sack)
	cc := tcpip.CongestionControlOption("cubic")
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &cc)
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &tcpip.TCPSendBufferSizeRangeOption{Min: 4096, Default: 1 << 20, Max: 16 << 20})
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &tcpip.TCPReceiveBufferSizeRangeOption{Min: 4096, Default: 1 << 20, Max: 16 << 20})
	ep := channel.New(4096, 1500, "")
	if err := s.CreateNIC(1, ep); err != nil {
		t.Fatalf("CreateNIC: %v", err)
	}
	if err := s.AddProtocolAddress(1, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: addr, PrefixLen: 24},
	}, stack.AddressProperties{}); err != nil {
		t.Fatalf("AddProtocolAddress: %v", err)
	}
	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}})
	return s, ep
}

// transfer sends size bytes from stack A to stack B through the pumps and
// returns A's (the sender's) TCP stats.
func transfer(t *testing.T, size int, delay time.Duration, pol policy) tcpip.TCPStats {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addrA := tcpip.AddrFrom4([4]byte{10, 0, 0, 1})
	addrB := tcpip.AddrFrom4([4]byte{10, 0, 0, 2})
	sa, ea := newStack(t, addrA)
	sb, eb := newStack(t, addrB)
	defer sa.Close()
	defer sb.Close()
	pump(ctx, ea, eb, delay, pol)
	pump(ctx, eb, ea, delay, nil)

	ln, err := gonet.ListenTCP(sb, tcpip.FullAddress{NIC: 1, Addr: addrB, Port: 8080}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
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
	c, err := gonet.DialContextTCP(dctx, sa, tcpip.FullAddress{NIC: 1, Addr: addrB, Port: 8080}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	buf := make([]byte, 64<<10)
	start := time.Now()
	for sent := 0; sent < size; sent += len(buf) {
		if _, err := c.Write(buf); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	c.Close()
	select {
	case n := <-got:
		if n != int64(size) {
			t.Fatalf("received %d bytes, want %d", n, size)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("transfer timed out")
	}
	t.Logf("transfer %d bytes in %v", size, time.Since(start))
	return sa.Stats().TCP
}

func logStats(t *testing.T, s tcpip.TCPStats) {
	t.Helper()
	t.Logf("sack_recovery=%d spurious=%d undo=%d timeouts=%d spurious_rto=%d dsack=%d retransmits=%d",
		s.SACKRecovery.Value(), s.SpuriousRecovery.Value(), s.SpuriousRecoveryUndo.Value(),
		s.Timeouts.Value(), s.SpuriousRTORecovery.Value(), s.SegmentsAckedWithDSACK.Value(), s.Retransmits.Value())
}

func TestSpuriousRecoveryUndoOnReorder(t *testing.T) {
	// Every 40th data segment arrives 8ms late: pure reordering, no loss.
	s := transfer(t, 8<<20, 10*time.Millisecond, func(n int) (time.Duration, bool) {
		if n%40 == 0 {
			return 8 * time.Millisecond, false
		}
		return 0, false
	})
	logStats(t, s)
	if s.SpuriousRecovery.Value() == 0 {
		t.Fatalf("reordering produced no spurious recovery; test is not exercising undo")
	}
	if s.SpuriousRecoveryUndo.Value() == 0 {
		t.Fatalf("spurious recoveries were detected but none undone")
	}
	if s.SpuriousRecoveryUndo.Value() > s.SpuriousRecovery.Value() {
		t.Fatalf("undo %d exceeds spurious %d", s.SpuriousRecoveryUndo.Value(), s.SpuriousRecovery.Value())
	}
}

func TestNoUndoOnRealLoss(t *testing.T) {
	// Every 300th data segment (first transmission or not) is dropped.
	s := transfer(t, 8<<20, 10*time.Millisecond, func(n int) (time.Duration, bool) {
		return 0, n%300 == 0
	})
	logStats(t, s)
	if s.SACKRecovery.Value() == 0 {
		t.Fatalf("drops produced no SACK recovery")
	}
	if s.SpuriousRecoveryUndo.Value() != 0 {
		t.Fatalf("real loss was undone %d times", s.SpuriousRecoveryUndo.Value())
	}
}

func TestNoUndoOnRTO(t *testing.T) {
	// Black-hole all data for ~1.5s mid-transfer to force RTOs.
	var start time.Time
	var once sync.Once
	s := transfer(t, 4<<20, 10*time.Millisecond, func(n int) (time.Duration, bool) {
		once.Do(func() { start = time.Now() })
		el := time.Since(start)
		return 0, el > 50*time.Millisecond && el < 1500*time.Millisecond
	})
	logStats(t, s)
	if s.Timeouts.Value() == 0 {
		t.Fatalf("black hole produced no RTO")
	}
	if s.SpuriousRecoveryUndo.Value() != 0 {
		t.Fatalf("RTO recovery was undone %d times", s.SpuriousRecoveryUndo.Value())
	}
}
