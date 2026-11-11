package p2p

import (
	"net"
	"time"
)

// UDP leg per-link state machine (docs/plans/p2p_udp_transport_design.md §4).
// Pure logic: no network I/O, no goroutines — the manager (Phase 1) executes
// the returned actions and feeds events back in. One FSM per TCP session
// (per proxy leg); role is fixed per session (dialer = active, listener = passive).
//
// States:
//
//	None ──enable──► Advertising ──DialPacket ok──► Channel ──gossip udpPort:P──► Negotiated
//	                                                                    │OnDataSent
//	                                                                    ▼
//	                             ┌──◄── first UDP inbound ── Pending ◄──┘
//	                             ▼              │ K 无回流且 TCP 正常
//	                           Active ──K 无回流──► Backoff ──R 到期──► Advertising
//	                             │                  (撤回宣告, 数据回 TCP)
//	                             └── gossip udp:true 消失 ──► 撤回
//
// Passive side: None ──peer gossip udp:true──► Advertising (+allocate port P)
//
//	──UDP inbound (双重条件: 宣告仍在 + 真收过 UDP)──► Active ──K──► Backoff

type udpLegPhase int

const (
	udpLegNone        udpLegPhase = iota
	udpLegAdvertising             // gossip 带 udp:true；主动端此时发起 DialPacket，被动端分配端口 P
	udpLegChannel                 // 主动端：UDP 通道已建
	udpLegNegotiated              // 主动端：已知对端 udpPort:P
	udpLegPending                 // 主动端：已发数据，等首个 UDP 回包
	udpLegActive                  // 双向 UDP 流通
	udpLegBackoff                 // 撤回宣告，退避 R 秒后重试
)

func (p udpLegPhase) String() string {
	switch p {
	case udpLegNone:
		return "none"
	case udpLegAdvertising:
		return "advertising"
	case udpLegChannel:
		return "channel"
	case udpLegNegotiated:
		return "negotiated"
	case udpLegPending:
		return "pending"
	case udpLegActive:
		return "active"
	case udpLegBackoff:
		return "backoff"
	}
	return "unknown"
}

// udpLegAction is a side-effect the manager must perform for a transition.
type udpLegAction int

const (
	legActNone           udpLegAction = iota
	legActStartAdvertise              // gossip 开始带 udp:true
	legActStopAdvertise               // gossip 撤回 udp:true
	legActDialChannel                 // 主动端：DialPacket 建 UDP 通道（标准 associate，透明零改动）
	legActCloseChannel                // 主动端：关闭 UDP 通道
	legActAllocatePort                // 被动端：分配端口 P 并登记 P→会话逻辑监听
	legActUnregisterPort              // 被动端：解除端口登记
)

type udpLegTimings struct {
	refluxTimeout time.Duration // K：激活/待激活态无 UDP 入流判死（单向不通/中途静默死亡）
	backoff       time.Duration // R：撤回后的重试间隔
}

var defaultUDPLegTimings = udpLegTimings{
	refluxTimeout: 10 * time.Second,
	backoff:       60 * time.Second,
}

type udpLegFSM struct {
	active  bool // true = 拨号侧（主动端）
	enabled bool // 配置 p2p_udp 开关
	timings udpLegTimings

	phase udpLegPhase

	peerCapable bool // 对端 gossip 仍有 udp:true
	peerPort    int  // 对端 udpPort（仅主动端用；sticky，瞬时为 0 不清除）

	localPort    int // 本端分配端口（仅被动端）
	channelReady bool

	sawUDPInbound  bool
	peerAddr       net.Addr // 被动端记住的对端返回地址（双重条件之②）
	refluxDeadline time.Time
	backoffUntil   time.Time
}

func newUDPLegFSM(active bool, enabled bool, timings udpLegTimings) *udpLegFSM {
	if timings.refluxTimeout <= 0 {
		timings.refluxTimeout = defaultUDPLegTimings.refluxTimeout
	}
	if timings.backoff <= 0 {
		timings.backoff = defaultUDPLegTimings.backoff
	}
	return &udpLegFSM{active: active, enabled: enabled, timings: timings, phase: udpLegNone}
}

func (f *udpLegFSM) Phase() udpLegPhase { return f.phase }

// ShouldAdvertise: gossip 载荷里应带 udp:true。
func (f *udpLegFSM) ShouldAdvertise() bool {
	switch f.phase {
	case udpLegAdvertising, udpLegChannel, udpLegNegotiated, udpLegPending, udpLegActive:
		return true
	}
	return false
}

// AdvertisePort: 被动端回带 udpPort:P（端口就绪且处于宣告/激活态才回带）。
func (f *udpLegFSM) AdvertisePort() (int, bool) {
	if f.active || f.localPort == 0 {
		return 0, false
	}
	if f.phase == udpLegAdvertising || f.phase == udpLegActive {
		return f.localPort, true
	}
	return 0, false
}

// SendPort: 主动端 datagram 目标端口（目标 IP 恒为 0.0.0.0）。
func (f *udpLegFSM) SendPort() (int, bool) {
	if !f.active || f.peerPort == 0 {
		return 0, false
	}
	switch f.phase {
	case udpLegNegotiated, udpLegPending, udpLegActive:
		return f.peerPort, true
	}
	return 0, false
}

// RouteDataViaUDP: 数据帧是否走 UDP 腿（主动端 Negotiated 起即可发送；
// 被动端仅 Active 且有对端返回地址时回 UDP，否则 TCP）。
func (f *udpLegFSM) RouteDataViaUDP() bool {
	if !f.active {
		return f.phase == udpLegActive && f.peerAddr != nil
	}
	_, ok := f.SendPort()
	return ok
}

// RouteProbeViaUDP: ping/pong 是否走 UDP。按方案 fallback 规则 6，
// 仅激活通路后走 UDP（测的就是数据真实通路）；回落/未激活期间自动回 TCP。
// 主动端 Pending 起（已发首个数据）任一 UDP 回包即可激活，故 Pending/Active 可用；
// 被动端必须已记住对端返回地址（Active）。
func (f *udpLegFSM) RouteProbeViaUDP() bool {
	if f.active {
		if f.peerPort == 0 {
			return false
		}
		return f.phase == udpLegPending || f.phase == udpLegActive
	}
	return f.phase == udpLegActive && f.peerAddr != nil
}

// PeerReturnAddr: 被动端记住的对端 UDP 返回地址。
func (f *udpLegFSM) PeerReturnAddr() net.Addr { return f.peerAddr }

// SawUDPInbound: 是否真收过对端 UDP（被动端回路径双重条件之②）。
func (f *udpLegFSM) SawUDPInbound() bool { return f.sawUDPInbound }

// SetLocalPort: 被动端完成端口分配后回告 FSM。
func (f *udpLegFSM) SetLocalPort(port int) {
	if !f.active {
		f.localPort = port
	}
}

// SetEnabled: 配置开关变化。关闭 = 全量撤回。
func (f *udpLegFSM) SetEnabled(now time.Time, enabled bool) []udpLegAction {
	if f.enabled == enabled {
		return nil
	}
	f.enabled = enabled
	if !enabled {
		return f.withdraw()
	}
	return nil
}

// OnGossip: 处理对端 gossip 的 udp/udpPort 字段。
func (f *udpLegFSM) OnGossip(now time.Time, peerUDP bool, peerPort int) []udpLegAction {
	if !f.enabled {
		return nil
	}
	if !peerUDP {
		f.peerCapable = false
		if f.active {
			// 主动端：宣告消失则端口失效，发送自然停（数据回 TCP）；
			// 不立即清状态，由回流超时兜底退避，避免 gossip 抖动造成 flap。
			f.peerPort = 0
			return nil
		}
		// 被动端：条件①消失，立即解除监听（在途帧丢失可容忍）。
		if f.phase == udpLegNone || f.phase == udpLegBackoff {
			return nil
		}
		return f.withdraw()
	}
	f.peerCapable = true
	if f.active {
		if peerPort > 0 {
			f.peerPort = peerPort
			if f.phase == udpLegChannel {
				f.phase = udpLegNegotiated
			}
		}
		if f.phase == udpLegNone {
			return f.enterAdvertising(now)
		}
		return nil
	}
	// passive
	if f.phase == udpLegNone {
		return f.enterAdvertising(now)
	}
	return nil
}

// OnChannelResult: 主动端 DialPacket 结果。失败不推进状态（manager 自行重试）。
func (f *udpLegFSM) OnChannelResult(now time.Time, ok bool) []udpLegAction {
	if !f.active || !ok {
		return nil
	}
	switch f.phase {
	case udpLegAdvertising, udpLegChannel:
		f.channelReady = true
		f.phase = udpLegChannel
		if f.peerPort > 0 {
			f.phase = udpLegNegotiated
		}
	}
	return nil
}

// OnDataSent: 主动端首个数据帧经 UDP 发出，进入待激活并起 K 计时。
func (f *udpLegFSM) OnDataSent(now time.Time) {
	if f.phase == udpLegNegotiated {
		f.phase = udpLegPending
		f.refluxDeadline = now.Add(f.timings.refluxTimeout)
	}
}

// OnUDPInbound: 收到对端 UDP datagram。
func (f *udpLegFSM) OnUDPInbound(now time.Time, addr net.Addr) []udpLegAction {
	switch f.phase {
	case udpLegNone, udpLegBackoff:
		return nil // 未协商/退避中的包：丢弃
	case udpLegAdvertising:
		if f.active {
			return nil // 主动端未发送前不应收到对端 UDP，忽略
		}
		// passive: 双重条件之②满足（之①由 Advertising 态保证）→ 激活
	case udpLegPending, udpLegActive:
		// active: 首个回包 / 持续回流
	default: // Channel / Negotiated
		return nil
	}
	f.sawUDPInbound = true
	f.peerAddr = addr
	f.phase = udpLegActive
	f.refluxDeadline = now.Add(f.timings.refluxTimeout)
	return nil
}

// Tick: 定时驱动（K 判死、R 退避到期、None 自愈进入宣告）。
func (f *udpLegFSM) Tick(now time.Time) []udpLegAction {
	switch f.phase {
	case udpLegPending, udpLegActive:
		if !f.refluxDeadline.IsZero() && !now.Before(f.refluxDeadline) {
			return f.fallback(now)
		}
	case udpLegBackoff:
		if now.Before(f.backoffUntil) {
			return nil
		}
		if !f.enabled || (!f.active && !f.peerCapable) {
			f.phase = udpLegNone
			return nil
		}
		return f.enterAdvertising(now)
	case udpLegNone:
		if f.enabled && (f.active || f.peerCapable) {
			return f.enterAdvertising(now)
		}
	}
	return nil
}

func (f *udpLegFSM) enterAdvertising(now time.Time) []udpLegAction {
	f.phase = udpLegAdvertising
	if f.active {
		return []udpLegAction{legActStartAdvertise, legActDialChannel}
	}
	return []udpLegAction{legActStartAdvertise, legActAllocatePort}
}

// withdraw: 全量撤回（停止宣告、关通道/解端口、清对端状态），落到 None。
func (f *udpLegFSM) withdraw() []udpLegAction {
	var acts []udpLegAction
	if f.ShouldAdvertise() {
		acts = append(acts, legActStopAdvertise)
	}
	if f.channelReady {
		acts = append(acts, legActCloseChannel)
		f.channelReady = false
	}
	if !f.active && f.localPort != 0 {
		acts = append(acts, legActUnregisterPort)
		f.localPort = 0
	}
	f.peerPort = 0
	f.sawUDPInbound = false
	f.peerAddr = nil
	f.refluxDeadline = time.Time{}
	f.phase = udpLegNone
	return acts
}

// fallback: 单向不通/中途静默死亡 → 撤回 + 退避 R 秒。
func (f *udpLegFSM) fallback(now time.Time) []udpLegAction {
	acts := f.withdraw()
	f.phase = udpLegBackoff
	f.backoffUntil = now.Add(f.timings.backoff)
	return acts
}

// ForceFallback lets the transport react to a detected data-path black hole
// (outbound active, inbound silent) before the regular K reflux timeout.
func (f *udpLegFSM) ForceFallback(now time.Time) []udpLegAction {
	return f.fallback(now)
}
