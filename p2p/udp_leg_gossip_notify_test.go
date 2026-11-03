package p2p

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"phaethon/frame"
)

// stubLegTransport blocks forever in Recv and no-ops sends — enough to host a
// udpLegTransport for tickLoop-driven tests.
type stubLegTransport struct {
	closed atomic.Bool
}

func (s *stubLegTransport) Send(frameType byte, payload []byte, isControl bool) error { return nil }
func (s *stubLegTransport) SendBatch(frames []frame.Frame) error                      { return nil }
func (s *stubLegTransport) Recv() (byte, []byte, error) {
	<-time.After(time.Hour)
	return 0, nil, errors.New("closed")
}
func (s *stubLegTransport) Close() error { s.closed.Store(true); return nil }

// 协商字段必须持续通知：FSM 离开 None 后 tickLoop 周期性触发 gossip 回调，
// 且 5s 间隔内不重复触发（回归：一次性会话建立 gossip 早于 FSM 就绪，
// udp/udpPort 永远发不出去）。
func TestUDPLegGossipNotifyWhileAdvertising(t *testing.T) {
	base := &stubLegTransport{}
	tr := newUDPLegTransport(base, true, nil)
	defer tr.Close()

	var calls atomic.Int32
	tr.SetGossipNotify(func() { calls.Add(1) })

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) && calls.Load() == 0 {
		time.Sleep(50 * time.Millisecond)
	}
	if calls.Load() == 0 {
		t.Fatalf("gossip notify never fired while advertising")
	}

	// 5s 节流：4s 观察窗内（首触之后）不应有第二次触发
	time.Sleep(3500 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Fatalf("gossip notify fired %d times, want 1 (5s throttle)", n)
	}
}
