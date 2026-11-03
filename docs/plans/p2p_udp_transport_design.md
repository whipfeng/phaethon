# P2P 数据面 UDP 传输设计（复用现有代理 UDP 通道）

**状态**: DRAFT（v2：按用户指正重写——不新造协议，复用 SOCKS5 UDP ASSOCIATE 等现有代理 UDP 通道承载 P2P 帧）
**创建**: 2026-11-03
**目标版本**: v1.0.0（首版）
**关联文档**: [p2p_mesh_data_frame_loss_design.md](p2p_mesh_data_frame_loss_design.md)、[htunnel_p2p_direct_mode.md](htunnel_p2p_direct_mode.md)、[reverse_udp_design.md](reverse_udp_design.md)

## 1. 背景与动机

用户诉求（可选项）：代理协议本身有 UDP 支持（如 SOCKS5 的 UDP ASSOCIATE），开启后 P2P 数据帧直接走这条现成的 UDP 通道，以获得真实 UDP 线上形态的性能收益。**不引入任何新线协议**——帧格式、握手、加密全部复用现有实现。

当前 P2P 链路的帧传输（`frame.FrameTransport`）三个实现全部承载在 TCP 流上（`frame.NewStreamTransport` / htunnel direct / mesh channel）。而代理客户端的 UDP 通道（`UDPDialer.DialPacket()`）和服务端的 UDP 中继（SOCKS5/Trojan UDP ASSOCIATE）**已经存在且生产可用**，缺的只是"P2P 帧走这条通道"的接驳。

收益：
1. **消除 TCP-over-TCP 队头阻塞**（仅当真 UDP 线上时成立，见 §3.4 协议对照）
2. **零新协议、零新依赖**：帧封装沿用现有 3 字节帧头；SOCKS5 UDP 封壳沿用现有实现
3. 对 mesh 数据面卡死症状（docs/issues/mesh-data-plane-stall.md）的解耦价值：UDP 腿与 TCP 腿故障域独立

## 2. 现状关键事实（调研实锤）

- **`FrameTransport` 已是 datagram 语义**（`frame/transport.go:20-32` 注释原文）："implementations MAY drop, duplicate or reorder frames"。**UDP 丢包/乱序契约天然满足，消费方零改动**。
- **方法集**：`Send(frameType, payload, isControl)` / `SendBatch([]Frame)` / `Recv()` / `Close()`；Send 由 peerWriteLoop 唯一 writer 串行调用。
- **帧格式**：`[frameType:1B][length:2B big-endian][payload]`（streamTransport 线上格式，UDP 版原样复用）。
- **帧类型**：控制帧 `Gossip(0x12)`/`LinkPing(0x14)`/`LinkPong(0x15)`；数据帧 `FrameMeshPacket(0x20)`。全部 fire-and-forget——gossip 周期广播、ping/pong 是测量，丢帧可容忍。
- **客户端 UDP 通道已全类型覆盖**：`UDPDialer.DialPacket() (net.PacketConn, error)`（`dialer/dialer.go:86`），SOCKS5=`dialer/socks5.go:19`→`Socks5UDPAssociate`（`dialer/socks5_udp.go:231`，支持多级级联）、Trojan=`dialer/trojan.go:98`、SS/VLESS/Hysteria2/h_tunnel/Direct 全有实现。
- **客户端 PacketConn 语义**：通用中继——每次 `WriteTo(payload, targetAddr)` 带 SOCKS5 UDP 封头（RSV/FRAG/ATYP/DST），目标地址任意。
- **服务端 UDP 中继已存在**：SOCKS5 `handleUDPAssociate`（`server/socks5.go:233`）→ `socks5UDPRelay`（:318）；Trojan `handleUDPAssociate`（`server/trojan.go:162`）；h_tunnel UDP 模式（`server/htunnel.go:382`）。中继生命周期跟随 TCP 控制连接（RFC 1928 语义，现有代码已实现）。
- **P2P 地址编码有先例**：TCP BIND PORT=2 的 DST.ADDR 携带 mesh address（`server/socks5.go:162` → `handleP2PConnection(conn, dstAddr)` → `p2p.HandleP2PConnection`，`p2p/p2p.go:2318`）。transport 变体入口 `p2p.HandleP2PTransport(t frame.FrameTransport, address string)`（`p2p/p2p.go:2336`）接受任意 FrameTransport。
- **入口（客户端）**：`P2PDialer.DialP2P() (frame.FrameTransport, error)`（`dialer/dialer.go:102`）。
- **无 NAT 穿透**：全部经代理中继，客户端与服务器地址互知，UDP 通道天然可达（除非运营商封 UDP）。

## 3. 设计

### 3.1 总体形态

```
┌─ 客户端 ───────────────────────────────────┐          ┌─ 服务器（代理节点）───────────────────────┐
│ DialP2P() with p2p_udp:                     │          │ UDP ASSOCIATE 请求 DST = P2P marker:      │
│  1. DialPacket() 建 UDP 通道                │  信令    │  ATYP=domain, ADDR=<mesh address>,        │
│     （TCP 控制连接 + UDP ASSOCIATE 握手，    │◄────────►│  PORT=2（与 BIND PORT=2 同语义）           │
│      全部现有代码）                           │          │    → 判定为 P2P 会话：不建 internet 中继    │
│  2. 用 marker 目标包装为 FrameTransport:     │  UDP     │    → frame.NewPacketTransport(relay leg)  │
│     Send = WriteTo(frame, p2pMarker)         │═════════►│    → p2p.HandleP2PTransport(t, address)   │
│     Recv = ReadFrom() 剥封头                 │  数据帧  │  中继收到 datagram:                        │
│  3. 失败 → 回退现有 TCP DialP2P 路径          │          │  DST=marker → 帧入 transport → p2p 会话    │
└─────────────────────────────────────────────┘          │  DST≠marker → 现有 internet 中继（不变）    │
                                                         └───────────────────────────────────────────┘
```

关键决策：

1. **新 FrameTransport 实现 `frame.PacketTransport`**：包装 `net.PacketConn`，Send/Recv 复用现有 3 字节帧头格式逐帧一 datagram。**不改 FrameTransport 接口、不改 p2p/mesh 任何消费方**。
2. **P2P 判定放在 UDP ASSOCIATE 请求的 DST**：客户端发 UDP ASSOCIATE 时 DST 填 mesh address（域名 ATYP，与 BIND PORT=2 同编码）+ PORT=2 marker。服务器见 marker → 该 association 整体是 P2P 会话（`HandleP2PTransport`），不建 internet 中继；datagram 内的 DST 字段则填占位值（0.0.0.0:0），服务器 P2P 分支忽略之。无 marker → 走现有 internet 中继，**行为零变化**。
3. **全部帧（控制+数据）都走 UDP 通道**：契约允许丢/乱序；gossip 周期广播自恢复、ping/pong 本就是测量。无需双通道、无需 TCP 信令腿。**TCP 控制连接仅作 association 生命周期载体**（现有语义：控制连接死 → 中继关 → p2p 会话关 → 走现有重连流程）。
4. **大包分片**：SOCKS5 UDP 单 datagram 保守上限 ~1400B；帧超限时发送侧 2 段式分片（首段带 fragID/total/index 头），接收侧重组，2s 超时整组丢弃（与 fire-and-forget 一致）。

### 3.2 改动面（全部在既有文件加分支，无新文件协议）

| 层 | 位置 | 改动 |
|----|------|------|
| 帧层 | `frame/` 新文件 | `PacketTransport` 实现（Send/SendBatch/Recv/Close + 分片重组），纯本地可单测 |
| 客户端 | `dialer/socks5.go` `DialP2P`（:81）等 | `p2p_udp` 开启时：DialPacket → 发 P2P-marker UDP ASSOCIATE（需 Socks5UDPAssociate 变体支持自定义 DST）→ 包 PacketTransport；失败回退现有 TCP 路径 |
| 服务端 | `server/socks5.go` `handleUDPAssociate`（:233） | 解析请求 DST，P2P marker → `frame.NewPacketTransport(relay leg)` + `p2p.HandleP2PTransport`；否则现有路径 |
| 配置 | `config.Proxy` | `p2p_udp *bool`，缺省 false（**可选项原则**：不开 = 与今天 bit 级一致） |
| admin | 会话列表 | 传输形态列显示 "udp"（复用 connID/出口形态辨识既有展示位） |

### 3.3 会话生命周期

- UDP association 的 TCP 控制连接存活 = P2P 会话存活（现有中继语义，不新造）。
- UDP 通道静默断流（运营商限速场景）：无线上信号，靠 gossip 超时/pingTracker ~10s 自判死链 + 最优路径重算（**现有机制原样复用**，见 mesh-data-plane-stall.md 根因链条③的修正结论——死链发现本来就是 ping/pong 的职责）。
- 重连：p2p 层现有重连循环重跑 DialP2P；`p2p_udp` 开启时每次重试先 UDP 后 TCP 回退。

### 3.4 各代理类型的真实线上形态（决定收益，必须诚实对照）

| 代理类型 | DialPacket 线上形态 | p2p_udp 收益 |
|----------|---------------------|--------------|
| **SOCKS5** | 真 UDP datagram（明文） | ✔ 全收益（队头阻塞消除、弱网吞吐） |
| **Shadowsocks** | 真 UDP datagram + AEAD 加密 | ✔ 全收益 + 线上加密 |
| **Hysteria2** | 真 UDP（QUIC）+ TLS | ✔ 全收益 + 线上加密 |
| **Trojan** | UDP 包封在 TLS 流里（`server/trojan.go:252` 从 TLS 连接读写） | ✘ **线上仍是 TCP**，不解决队头阻塞，仅省帧封套开销——不建议启用 |
| **h_tunnel** | UDP over HTTP 流 | ✘ 同上，线上 TCP |
| **VLESS** | 依传输层而定 | 视部署，默认不建议 |

**结论**：`p2p_udp` 的收益取决于代理类型的 UDP 通道是不是真 UDP。首版推荐在 SOCKS5（内网可信链路，明文可接受）或 Shadowsocks/Hysteria2 腿上启用；Trojan/h_tunnel 腿上启用无害但无性能收益（仍可用作功能验证）。

### 3.5 级联链

`Socks5UDPAssociate` 已支持多级级联（`dialer/socks5_udp.go:231-249`，递归建立下一跳 UDP ASSOCIATE）。P2P 帧经级联链时逐跳 UDP——中间跳若某环是 TCP 线上形态则该环收益失效。首版不要求逐环最优，按整链最后一跳的 marker 判定即可（中级 SOCKS5 服务器透传 datagram，与现有 internet UDP 中继同路径）。

## 4. 配置

```json
{
  "proxies": [{
    "name": "qg",
    "type": "socks5",
    "p2p": true,
    "p2p_udp": true
  }]
}
```

- `p2p_udp` 缺省 false；仅对 UDP 线上形态为真的代理类型有意义（§3.4）。
- 服务端无需配置：association 请求里的 marker 即声明，老版本服务器收到 P2P-marker UDP ASSOCIATE 会按普通中继处理（DST 解析失败自然拒绝）——**天然向后不兼容安全**（项目未发布，不涉及兼容 shim）。

## 5. 风险与对策

| 风险 | 等级 | 对策 |
|------|------|------|
| 运营商 UDP 限速/封锁 | 高 | DialPacket/握手失败自动回退现有 TCP DialP2P；运行中静默断流靠现有 ping/pong 死链发现 + 重连（重连时再试 UDP）；per-proxy 开关便于按链路启用 |
| SOCKS5 UDP 明文 | 中 | 内网/可信链路可接受（与现有 SOCKS5 P2P TCP 明文同级）；要加密走 Shadowsocks/Hysteria2 腿 |
| 分片重组复杂度 | 中 | 仅数据帧触发、2 段式、2s 超时丢弃；保守 1400B 阈值不依赖 PMTU 动态探测 |
| UDP 单 datagram 乱序导致分片重组失败率上升 | 低 | 丢帧本来就容忍（内层 TCP 重传）；重组失败 = 丢一帧，契约内 |
| 服务器 UDP 中继被扫 | 低 | P2P marker 需 association 级握手成立；无关 datagram 走现有路径 |

## 6. 与 QUIC 方案的关系（v1 版方案处置）

v1 版方案（quicTransport + 自签证书 pin + RFC 9221 datagram）**撤销**：自造协议层与"复用现有协议"的诉求相悖，且 SOCKS5-UDP 明文/SS-UDP/Hysteria2 已覆盖"加密/不加密 × 真 UDP"全组合。若未来某代理类型无 UDP 通道又需要真 UDP，再考虑 QUIC——当前不存在这个缺口。

## 7. 实施步骤（分期，每期独立可交付可回退）

**Phase 0 — PacketTransport 内核（纯本地）**
1. `frame/packet_transport.go`：`NewPacketTransport(pc net.PacketConn, peer net.Addr) FrameTransport`——3 字节帧头逐帧一 datagram + 分片重组 + Close 语义。
2. 本地单测：内存 PacketConn 对，验证契约（丢/乱/重容忍、分片边界、SendBatch 语义）。

**Phase 1 — SOCKS5 端到端（推荐首条腿）**
3. 客户端：`Socks5UDPAssociate` 加 `dstAddr/dstPort` 参数变体（P2P-marker 编码 mesh address）；`DialP2P` 加 `p2p_udp` 分支：DialPacket 成功→PacketTransport，失败→现有 TCP 路径，info 日志记形态。
4. 服务端：`handleUDPAssociate` 解析请求 DST；P2P marker → PacketTransport over relay leg → `p2p.HandleP2PTransport`；datagram 读写复用 relay 现有 clientLn 通路。
5. 配置 `p2p_udp` + admin 传输形态展示。

**Phase 2 — 扩展类型 + 实测**
6. Shadowsocks/Hysteria2 的 DialP2P 同分支（DialPacket 已存在，主要是 marker 协商方式按各协议适配）。
7. VM↔QG 实链路启用跑 iperf3 基线对比（TCP 腿 vs UDP 腿吞吐/RTT/丢包），数据入 docs/issues/ 或 runbooks。
8. 跨平台验证（Windows wintun + Linux tun）。

**Phase 3（后续可选）**
- Trojan/h_tunnel 腿上启用（功能可用、无线上收益，仅当想统一代码路径时做）。

## 8. 验证标准

- 契约层：PacketTransport 全套单测过（含丢/乱/重、分片、Close）。
- 回归层：`p2p_udp` 关闭或未配置时，全部行为与现状 bit 级一致（现有 e2e 套件原样通过）。
- 功能层：开启后 VM↔QG mesh 数据面正常（域名路由 + VIP + IPIP），admin 会话显示 udp 形态。
- 回退层：UDP 通道建不起来（防火墙封 UDP 模拟）时自动落 TCP，业务无感。
- 无 TUN 绕排他性：验证必须走 wintun/tun 全链路（项目铁律）。

## 9. 未决问题

1. P2P-marker 的精确编码（associate 请求 DST 的域名格式与 BIND PORT=2 的 address 编码是否完全一致，Phase 1 第 3 步前核对 `socks5Handshake` 的地址编码）。
2. 分片阈值定值（首版 1400B 保守值，实测后可调）。
3. UDP 腿的链路质量（SRTT/loss）首版沿用 ping/pong 现有测量，不改最优路径计算。
