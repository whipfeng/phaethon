package p2p

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"phaethon/frame"
)

// stubLegTransport blocks forever in Recv and no-ops sends — enough to host a
// udpLegTransport for state-notify tests.
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

// 宣告态变化必须立即触发 mesh 补发 gossip（设计 §3 R2）：进入宣告态发一次，
// 撤回再发一次；不再周期重发（旧 5s notify 机制已删除）。
func TestUDPLegStateNotifyOnAdvertiseTransitions(t *testing.T) {
	base := &stubLegTransport{}
	tr := newUDPLegTransport(base, false, nil, "")
	defer tr.Close()

	var calls atomic.Int32
	tr.SetStateNotify(func() { calls.Add(1) })

	// 对端宣告 udp:true → 本端 FSM None→Advertising（同步路径，无需等待）
	tr.onPeerGossip(true, 0)
	if n := calls.Load(); n != 1 {
		t.Fatalf("state notify fired %d times entering advertising, want 1", n)
	}

	// 对端撤回 udp:false → 本端 withdraw 退出宣告态 → 再触发一次
	tr.onPeerGossip(false, 0)
	if n := calls.Load(); n != 2 {
		t.Fatalf("state notify fired %d times after withdraw, want 2", n)
	}
}
