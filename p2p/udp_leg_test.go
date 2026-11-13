package p2p

import (
	"net"
	"reflect"
	"testing"
	"time"
)

var udpLegTestBase = time.Unix(1_700_000_000, 0)

func udpLegAddr() net.Addr {
	return &net.UDPAddr{IP: net.ParseIP("100.64.0.1"), Port: 51234}
}

func hasAction(acts []udpLegAction, want udpLegAction) bool {
	for _, a := range acts {
		if a == want {
			return true
		}
	}
	return false
}

// 主动端完整通路：宣告 → 建通道 → 协商端口 → 发数据 → 首个回包 → 激活。
func TestUDPLegActiveHappyPath(t *testing.T) {
	f := newUDPLegFSM(true, false, udpLegTimings{})
	t0 := udpLegTestBase

	if acts := f.SetEnabled(t0, true); acts != nil {
		t.Fatalf("SetEnabled(true) actions = %v, want nil (waits for Tick)", acts)
	}
	acts := f.Tick(t0)
	if !hasAction(acts, legActStartAdvertise) || !hasAction(acts, legActDialChannel) {
		t.Fatalf("Tick actions = %v, want start-advertise + dial", acts)
	}
	if !f.ShouldAdvertise() {
		t.Fatalf("ShouldAdvertise = false in advertising phase")
	}

	// DialPacket 失败：留在宣告态，由 manager 重试
	if acts := f.OnChannelResult(t0.Add(time.Second), false); acts != nil || f.Phase() != udpLegAdvertising {
		t.Fatalf("channel failure: acts=%v phase=%v", acts, f.Phase())
	}
	f.OnChannelResult(t0.Add(time.Second), true)
	if f.Phase() != udpLegChannel {
		t.Fatalf("phase = %v, want channel", f.Phase())
	}

	// 对端先回 udp:true 无端口 → 仍等端口
	f.OnGossip(t0.Add(2*time.Second), true, 0)
	if f.Phase() != udpLegChannel {
		t.Fatalf("phase = %v, want channel before udpPort known", f.Phase())
	}
	f.OnGossip(t0.Add(2*time.Second), true, 32458)
	if f.Phase() != udpLegNegotiated {
		t.Fatalf("phase = %v, want negotiated", f.Phase())
	}
	if port, ok := f.SendPort(); !ok || port != 32458 {
		t.Fatalf("SendPort = %d,%v, want 32458,true", port, ok)
	}
	// 直连模型：协商到端口后先用 ping 探测，数据仍走 TCP，确认回包后才切
	if f.RouteDataViaUDP() {
		t.Fatalf("RouteDataViaUDP = true in negotiated, want false (probe first)")
	}
	if !f.RouteProbeViaUDP() {
		t.Fatalf("RouteProbeViaUDP = false in negotiated, want true")
	}

	// 未发数据前收到对端 UDP：非法，忽略
	f.OnUDPInbound(t0.Add(3*time.Second), udpLegAddr())
	if f.Phase() != udpLegNegotiated {
		t.Fatalf("phase = %v, want negotiated (unsolicited inbound ignored)", f.Phase())
	}

	f.OnDataSent(t0.Add(3 * time.Second))
	if f.Phase() != udpLegPending {
		t.Fatalf("phase = %v, want pending", f.Phase())
	}
	// Pending 起 ping/pong 也走 UDP：pong 回包可充当"首个 UDP 回包"激活通路
	if !f.RouteProbeViaUDP() {
		t.Fatalf("RouteProbeViaUDP = false in pending, want true")
	}
	f.OnUDPInbound(t0.Add(4*time.Second), udpLegAddr())
	if f.Phase() != udpLegActive {
		t.Fatalf("phase = %v, want active", f.Phase())
	}
}

// fallback 2：单向不通——待激活 K 秒无 UDP 回流 → 撤回宣告、退避 R 秒后重试。
func TestUDPLegActivePendingTimeoutFallsBack(t *testing.T) {
	f := newUDPLegFSM(true, true, udpLegTimings{})
	t0 := udpLegTestBase

	f.Tick(t0)
	f.OnChannelResult(t0, true)
	f.OnGossip(t0, true, 40001)
	f.OnDataSent(t0)

	// K 内不判死
	if acts := f.Tick(t0.Add(9 * time.Second)); acts != nil {
		t.Fatalf("Tick within K: acts = %v, want nil", acts)
	}
	// K 超时 → fallback
	acts := f.Tick(t0.Add(10 * time.Second))
	if !hasAction(acts, legActStopAdvertise) || !hasAction(acts, legActCloseChannel) {
		t.Fatalf("fallback actions = %v, want stop-advertise + close-channel", acts)
	}
	if f.Phase() != udpLegBackoff {
		t.Fatalf("phase = %v, want backoff", f.Phase())
	}
	if f.RouteDataViaUDP() {
		t.Fatalf("RouteDataViaUDP = true in backoff, want false")
	}

	// 退避期内停留
	if acts := f.Tick(t0.Add(10*time.Second + 59*time.Second)); acts != nil || f.Phase() != udpLegBackoff {
		t.Fatalf("within backoff: acts=%v phase=%v", acts, f.Phase())
	}
	// R 到期 → 重新宣告 + 重拨
	acts = f.Tick(t0.Add(10*time.Second + 60*time.Second))
	if !hasAction(acts, legActStartAdvertise) || !hasAction(acts, legActDialChannel) {
		t.Fatalf("backoff expiry actions = %v, want start-advertise + dial", acts)
	}
	if f.Phase() != udpLegAdvertising {
		t.Fatalf("phase = %v, want advertising", f.Phase())
	}
}

// fallback 4：中途静默死亡——激活态 K 秒无回流，同 3 判定。
func TestUDPLegActiveMidStreamDeath(t *testing.T) {
	f := newUDPLegFSM(true, true, udpLegTimings{})
	t0 := udpLegTestBase
	addr := udpLegAddr()

	f.Tick(t0)
	f.OnChannelResult(t0, true)
	f.OnGossip(t0, true, 40002)
	f.OnDataSent(t0)
	f.OnUDPInbound(t0, addr)
	if f.Phase() != udpLegActive {
		t.Fatalf("setup: phase = %v, want active", f.Phase())
	}

	last := t0
	for i := 0; i < 5; i++ {
		last = last.Add(3 * time.Second)
		f.OnUDPInbound(last, addr)
		if f.Phase() != udpLegActive {
			t.Fatalf("after inbound %d: phase = %v, want active", i, f.Phase())
		}
	}
	// 最后一次回流后再过 K → 判死
	if acts := f.Tick(last.Add(10 * time.Second)); f.Phase() != udpLegBackoff || acts == nil {
		t.Fatalf("after silent K: phase=%v acts=%v, want backoff", f.Phase(), acts)
	}
}

// fallback 5：对端关开关。
// 主动端：宣告字段消失 → 端口立即失效、数据回 TCP；状态由回流超时兜底退避（不 flap）。
func TestUDPLegPeerSwitchOffActive(t *testing.T) {
	f := newUDPLegFSM(true, true, udpLegTimings{})
	t0 := udpLegTestBase

	f.Tick(t0)
	f.OnChannelResult(t0, true)
	f.OnGossip(t0, true, 40003)
	f.OnDataSent(t0)
	f.OnUDPInbound(t0, udpLegAddr())
	if f.Phase() != udpLegActive {
		t.Fatalf("setup: phase = %v, want active", f.Phase())
	}

	f.OnGossip(t0.Add(time.Second), false, 0)
	if _, ok := f.SendPort(); ok {
		t.Fatalf("SendPort still ok after peer switch-off")
	}
	if f.RouteDataViaUDP() || f.RouteProbeViaUDP() {
		t.Fatalf("route flags true after peer switch-off, want false")
	}
	// 不再回流 → K 秒后退避
	if f.Tick(t0.Add(9*time.Second)) != nil || f.Phase() != udpLegActive {
		t.Fatalf("before reflux deadline: phase=%v", f.Phase())
	}
	if f.Tick(t0.Add(10*time.Second)) == nil || f.Phase() != udpLegBackoff {
		t.Fatalf("after reflux deadline: phase=%v, want backoff", f.Phase())
	}
}

// fallback 5：被动端：宣告消失 → 立即解除端口登记。
func TestUDPLegPeerSwitchOffPassive(t *testing.T) {
	f := newUDPLegFSM(false, true, udpLegTimings{})
	t0 := udpLegTestBase

	acts := f.OnGossip(t0, true, 0)
	if !hasAction(acts, legActAllocatePort) {
		t.Fatalf("actions = %v, want allocate-port", acts)
	}
	f.SetLocalPort(40004)
	if port, ok := f.AdvertisePort(); !ok || port != 40004 {
		t.Fatalf("AdvertisePort = %d,%v, want 40004,true", port, ok)
	}

	acts = f.OnGossip(t0.Add(time.Second), false, 0)
	if !hasAction(acts, legActStopAdvertise) || !hasAction(acts, legActUnregisterPort) {
		t.Fatalf("withdraw actions = %v, want stop-advertise + unregister-port", acts)
	}
	if f.Phase() != udpLegNone {
		t.Fatalf("phase = %v, want none", f.Phase())
	}
	if _, ok := f.AdvertisePort(); ok {
		t.Fatalf("AdvertisePort still ok after withdraw")
	}

	// 解除后迟到的 UDP：双重条件不再满足，丢弃
	f.OnUDPInbound(t0.Add(2*time.Second), udpLegAddr())
	if f.SawUDPInbound() || f.Phase() != udpLegNone {
		t.Fatalf("late inbound: sawInbound=%v phase=%v", f.SawUDPInbound(), f.Phase())
	}
	// 对端已关 → 不自愈回宣告
	if acts := f.Tick(t0.Add(3 * time.Second)); acts != nil || f.Phase() != udpLegNone {
		t.Fatalf("tick after peer off: acts=%v phase=%v", acts, f.Phase())
	}
}

// 被动端完整通路 + 双重条件：必须先见宣告，且真收过 UDP，才激活记地址。
func TestUDPLegPassiveHappyPath(t *testing.T) {
	f := newUDPLegFSM(false, true, udpLegTimings{})
	t0 := udpLegTestBase
	addr := udpLegAddr()

	// 条件②先到（收到 UDP 但从未见宣告）：不激活
	f.OnUDPInbound(t0, addr)
	if f.SawUDPInbound() || f.Phase() != udpLegNone {
		t.Fatalf("inbound before declaration: sawInbound=%v phase=%v", f.SawUDPInbound(), f.Phase())
	}

	acts := f.OnGossip(t0, true, 0)
	if !hasAction(acts, legActStartAdvertise) || !hasAction(acts, legActAllocatePort) {
		t.Fatalf("actions = %v, want start-advertise + allocate-port", acts)
	}
	if f.Phase() != udpLegAdvertising {
		t.Fatalf("phase = %v, want advertising", f.Phase())
	}

	// 端口未就绪前不回带
	if _, ok := f.AdvertisePort(); ok {
		t.Fatalf("AdvertisePort ok before SetLocalPort")
	}
	f.SetLocalPort(40005)
	if port, ok := f.AdvertisePort(); !ok || port != 40005 {
		t.Fatalf("AdvertisePort = %d,%v, want 40005,true", port, ok)
	}

	f.OnUDPInbound(t0.Add(time.Second), addr)
	if f.Phase() != udpLegActive {
		t.Fatalf("phase = %v, want active", f.Phase())
	}
	if !f.SawUDPInbound() {
		t.Fatalf("SawUDPInbound = false after inbound")
	}
	if f.PeerReturnAddr() == nil || f.PeerReturnAddr().String() != addr.String() {
		t.Fatalf("PeerReturnAddr = %v, want %v", f.PeerReturnAddr(), addr)
	}
	if !f.RouteDataViaUDP() || !f.RouteProbeViaUDP() {
		t.Fatalf("route flags false in active, want true")
	}

	// K 无回流 → 撤回 + 退避
	acts = f.Tick(t0.Add(time.Second + 10*time.Second))
	if !hasAction(acts, legActUnregisterPort) || f.Phase() != udpLegBackoff {
		t.Fatalf("fallback: acts=%v phase=%v", acts, f.Phase())
	}
	// R 到期且对端仍宣告 → 重新宣告 + 重新分配
	acts = f.Tick(t0.Add(time.Second + 10*time.Second + 60*time.Second))
	if !hasAction(acts, legActAllocatePort) || f.Phase() != udpLegAdvertising {
		t.Fatalf("backoff expiry: acts=%v phase=%v", acts, f.Phase())
	}
	f.SetLocalPort(40006)
	if port, ok := f.AdvertisePort(); !ok || port != 40006 {
		t.Fatalf("re-advertise port = %d,%v", port, ok)
	}
}

// 开关关闭 = 全量撤回，任何事件都不再推进。
func TestUDPLegDisabled(t *testing.T) {
	f := newUDPLegFSM(false, true, udpLegTimings{})
	t0 := udpLegTestBase

	f.OnGossip(t0, true, 0)
	f.SetLocalPort(40007)
	f.OnUDPInbound(t0, udpLegAddr())
	if f.Phase() != udpLegActive {
		t.Fatalf("setup: phase = %v, want active", f.Phase())
	}

	acts := f.SetEnabled(t0, false)
	if !hasAction(acts, legActStopAdvertise) || !hasAction(acts, legActUnregisterPort) {
		t.Fatalf("disable actions = %v, want stop-advertise + unregister-port", acts)
	}
	if f.Phase() != udpLegNone {
		t.Fatalf("phase = %v, want none", f.Phase())
	}

	// 关闭后 gossip/UDP/Tick 全部无效
	f.OnGossip(t0.Add(time.Second), true, 0)
	f.OnUDPInbound(t0.Add(time.Second), udpLegAddr())
	f.Tick(t0.Add(2 * time.Second))
	if f.Phase() != udpLegNone {
		t.Fatalf("phase after events while disabled = %v, want none", f.Phase())
	}

	// 从未开启的 FSM：Tick 无动作
	g := newUDPLegFSM(true, false, udpLegTimings{})
	if acts := g.Tick(t0); acts != nil {
		t.Fatalf("disabled tick actions = %v, want nil", acts)
	}
}

// 显式校验动作序列的稳定性（无重复/丢动作）。
func TestUDPLegActionSequence(t *testing.T) {
	f := newUDPLegFSM(false, true, udpLegTimings{})
	t0 := udpLegTestBase
	acts := f.OnGossip(t0, true, 0)
	want := []udpLegAction{legActStartAdvertise, legActAllocatePort}
	if !reflect.DeepEqual(acts, want) {
		t.Fatalf("actions = %v, want %v", acts, want)
	}
}
