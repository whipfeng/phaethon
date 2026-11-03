# P2P 数据面 UDP 传输（QUIC）设计

**状态**: DRAFT
**创建**: 2026-11-03
**目标版本**: v1.0.0（首版）
**关联文档**: [p2p_mesh_data_frame_loss_design.md](p2p_mesh_data_frame_loss_design.md)、[htunnel_p2p_direct_mode.md](htunnel_p2p_direct_mode.md)、[reverse_udp_design.md](reverse_udp_design.md)

## 1. 背景与动机

当前所有 P2P mesh 链路的帧传输（`frame.FrameTransport`）有三个实现，全部承载在 TCP 之上：

| 实现 | 位置 | 底层 |
|------|------|------|
| `streamTransport` | `frame/transport.go:63` | 裸 TCP（SOCKS5/Trojan BIND PORT=2） |
| `htunnelDirectTransport` | `dialer/htunnel_direct.go` | HTTP POST 发 / GET 长轮询收 |
| `meshChannelTransport` | `server/htunnel_mesh.go:234` | server 侧 meshOut 队列（4096 帧） |

用户诉求（可选项，非强制）：开启 UDP 支持后，P2P 数据帧可走 UDP 以提升性能。具体收益：

1. **消除 TCP-over-TCP 队头阻塞**：内层 gVisor netstack 已在链路两端终结 TCP；外层再套可靠流协议，单帧丢失阻塞整条链路。UDP 数据腿无此耦合。
2. **弱网/高 RTT 下吞吐**：QUIC 的拥塞控制（BBR）与 0-RTT 建连优于双重 TCP 语义叠加。
3. **对 mesh 数据面卡死症状的解耦价值**（docs/issues/mesh-data-plane-stall.md）：htunnel 链路的出站停滞卡死整腿；UDP 腿与 TCP 信令腿天然独立，单腿死不会静默拖死全部数据面。

## 2. 现状关键事实（调研实锤）

- **`FrameTransport` 已是 datagram 语义**（`frame/transport.go:20-32` 接口注释原文）："implementations MAY drop, duplicate or reorder frames (all P2P frame consumers tolerate this)"。**UDP 化的契约口子已预留**，无需改任何消费方。
- **方法集**：`Send(frameType, payload, isControl)` / `SendBatch([]Frame)` / `Recv()` / `Close()`。Send 由 peerWriteLoop 唯一 writer 串行调用；Recv 必须**控制帧优先**（gossip/ping/pong 不能被数据帧饿死）。
- **帧类型**：控制帧 `Gossip(0x12)`/`LinkPing(0x14)`/`LinkPong(0x15)`（0x10-0x1F）；数据帧仅 `FrameMeshPacket(0x20)`。全部 fire-and-forget，无 seq/ack（见 [p2p_mesh_data_frame_loss_design.md](p2p_mesh_data_frame_loss_design.md)：丢包靠内层 TCP 重传 + gossip 周期广播弥补）。
- **数据帧载荷 = gVisor netstack IP 包**，内层 TCP/UDP 由两端 netstack 终结——**数据帧丢帧安全**（内层 TCP 自恢复；内层 UDP 本就容忍丢失）。
- **quic-go 已在依赖树**：`github.com/apernet/quic-go v0.54.1-0.20251024023933`（hysteria2 引入，当前 indirect）。升级 direct 即可用，支持 RFC 9221 QUIC Datagram（`SendDatagram`/`ReceiveDatagram`）。
- **无 NAT 穿透**：所有 P2P 连接经代理服务器中继（SOCKS5/Trojan/h_tunnel），客户端与服务器地址互相已知 → **无打洞需求**，UDP 直连服务器监听口即可。
- **无统一 P2P 加密层**：传输层加密依附底层协议（Trojan=TLS、h_tunnel=可选 XChaCha20 AEAD、SOCKS5 P2P=**明文**）。QUIC 的 TLS 1.3 顺带补齐 SOCKS5 路径的加密缺口。
- **入口（客户端）**：`P2PDialer.DialP2P() (frame.FrameTransport, error)`（`dialer/dialer.go:102`），各代理类型实现于 `dialer/socks5.go:81` / `dialer/trojan.go:78` / `dialer/htunnel.go:236`。
- **入口（服务端）**：`server/socks5.go:162` / `server/trojan.go:120` / `server/htunnel.go:502` → `handleP2PConnection` → `p2p.HandleP2PConnection`（`p2p/p2p.go:2318`）→ `frame.NewStreamTransport(conn)` → `GlobalP2PManager.HandleP2PTransport(t, address)`。transport 变体入口 `p2p.HandleP2PTransport`（`p2p/p2p.go:2336`）直接接受任意 `FrameTransport`。
- **出站 QoS**：4 级优先级队列（control > DNS > TCP-ACK > 数据）+ DRR（`p2p/p2p.go:339-343, 769-841`），在 peerWriteLoop 调 Send 之前，与 transport 实现无关，UDP transport 自动继承。

## 3. 设计

### 3.1 总体形态

```
┌─ 客户端 ──────────────────────────────┐         ┌─ 服务器（代理节点）──────────────────┐
│ DialP2P():                             │         │ TCP 信令腿（现有，不动）:            │
│  1. 现有 TCP transport 建链（SOCKS5    │  信令   │  SOCKS5/Trojan/h_tunnel 握手后      │
│     BIND PORT=2 / h_tunnel MESH）     │◄───────►│  → handleP2PConnection              │
│  2. 经信令腿发送 QUIC-UPGRADE 请求:   │         │     → HandleP2PTransport            │
│     {udp 目标端口, cert指纹, token}   │         │                                     │
│  3. UDP 直连服务器 QUIC 监听口,        │  QUIC   │ UDP 监听口（同一端口）:             │
│     TLS1.3 握手（pin 第 2 步指纹）    │═════════►│  quic.Listener                      │
│  4. QUIC 腿就绪 → 帧走 QUIC；         │  UDP    │  token → address 映射 →             │
│     信令腿降级为 fallback 探测        │         │  frame.NewQuicTransport(conn)       │
│                                        │         │  → p2p.HandleP2PTransport           │
└────────────────────────────────────────┘         └─────────────────────────────────────┘
```

核心决策：

1. **QUIC 作为第四条 FrameTransport 实现**（`frame.NewQuicTransport(quic.Connection)`），不改 FrameTransport 接口、不改任何消费方。
2. **信令走现有 TCP 腿**（复用已认证会话协商 QUIC 参数），UDP 仅承载升级后的帧流。无 PKI：服务器启动生成自签证书，证书 SHA-256 指纹经信令腿下发，客户端 QUIC TLS 握手时 pin 校验。h_tunnel 路径信令有 AEAD 加持；SOCKS5 路径信令为明文——但 pin 指纹仍使 UDP 腿不可被中间人仿冒（优于当前 SOCKS5 P2P 全明文状态）。
3. **控制帧走可靠 QUIC 流，数据帧走 QUIC Datagram（RFC 9221）**：
   - 单条双向 control stream：承载 gossip/ping/pong（低频、必须可靠、直接影响路由正确性）。
   - Datagram channel：承载 FrameMeshPacket。天然满足"丢/重/乱序可容忍"契约，彻底无队头阻塞。
   - `Recv()` 实现为优先 drain control stream，再读 datagram——满足控制帧优先要求。
4. **会话归属用 token**：服务器从信令腿拿到 address+token，UDP 到达后用 token 匹配；未匹配 token 的 QUIC 连接直接拒绝。

### 3.2 帧封装

**Control stream**（可靠，按现有 3 字节帧头格式原样转发，无新协议）：

```
[frameType:1B][length:2B big-endian][payload]
```

**Datagram**（每帧一个 datagram，不超过协商 MaxDatagramFrameSize）：

```
[frameType:1B = 0x20][length:2B][payload]
```

载荷超过当前可发送尺寸时，**发送侧分片**（轻量 2 段式，首段 2B fragID + 1B total + 2B index，续段同头），接收侧重组（按 fragID 聚合，单帧 2s 超时丢弃整组——与 fire-and-forget 语义一致）。PMTU 由 QUIC 栈管理，分片上限跟随 `quic.Connection.RemoteAddr()` 实际可用尺寸动态收缩，不硬编码。

### 3.3 配置

挂在现有 `Proxy` 配置面（与 `p2p` 开关同级，继承 per-proxy 粒度）：

```json
{
  "proxies": [{
    "name": "qg",
    "type": "trojan",
    "p2p": true,
    "p2p_udp": true
  }]
}
```

- `p2p_udp` 缺省 = false（**可选项原则**：不开 = 行为与今天完全一致）。
- 语义：`p2p_udp=true` 且 `p2p=true` 时，该代理的 P2P 建链尝试 QUIC 升级；升级失败（UDP 不可达/被拒/握手超时 3s）自动回退纯 TCP transport，记一条 info 日志。回退不是错误路径，是常态路径之一（运营商 UDP 限速/封锁场景）。

### 3.4 服务端 UDP 监听

- Trojan/SOCKS5 代理：在现有 TCP 监听同一端口加 UDP 监听（`net.ListenUDP` → `quic.Listen`），与 SOCKS5 UDP ASSOCIATE 不冲突（QUIC 首包即 crypto 帧，按首字节区分于 SOCKS5-UDP 封头）。
- h_tunnel：UDP 监听口独立配置项（h_tunnel 本身是 HTTP 端口无 UDP 面），协商时经信令腿告知客户端。
- 服务端 `p2p_udp` 开关同样配置化；两侧任一侧关闭 → 信令腿协商自然失败 → TCP 回退。

### 3.5 生命周期

- QUIC 腿建立后：TCP 信令腿**保留但降载**——不承载帧，仅作存活探测与重协商通道（每 15s 应用层 ping，失败 N 次判定腿死）。这样 UDP 被运营商静默断流时有现成的可靠通道可感知并回退，而不是等到 gossip 超时才发现。
- QUIC 腿断开：若 TCP 信令腿仍活 → 重协商重连 QUIC；信令腿也死 → 走现有会话关闭/重连流程（pingTracker/最优路径重算，机制不变）。
- **不改** peerWriteLoop / QoS 队列 / mesh 路由 / gossip 任何逻辑——它们只面向 FrameTransport 接口。

## 4. 与 mesh 数据面卡死问题的关系（非本方案承诺）

- 本方案不治本（B 触发源仍未实锤，见 docs/issues/mesh-data-plane-stall.md），但消除了"单条 TCP 传输腿内部停滞 → 全链路数据面静默卡死"这一类故障形态：UDP 腿与 TCP 信令腿独立，任一条死亡可被另一条立即感知。
- 阶段二精准 dump 阶梯照常部署取证，与本方案正交。

## 5. 风险与对策

| 风险 | 等级 | 对策 |
|------|------|------|
| 运营商 UDP 限速/封锁（国内网络常见，国际链路偶发） | 高 | 升级失败 3s 回退 TCP；信令腿降级探测可在运行中感知静默断流再回退；`p2p_udp` per-proxy 开关便于按链路质量启用 |
| 分片重组引入额外内存/复杂度 | 中 | 分片仅数据帧用、上限跟随 PMTU（典型 ≤ 3 段）；2s 超时整组丢弃，不复用 TCP 语义 |
| QUIC 依赖升级（indirect → direct）引入版本冲突 | 中 | hysteria2 已用同 fork 验证兼容性；升级后跑全量 e2e |
| 服务器 UDP 监听被扫/被放大攻击利用 | 低 | token 未匹配即拒；TLS 1.3 握手本身即抗放大（客户端首包验证后才放大响应） |
| Windows wintun/防火墙对 UDP 高并发限制 | 中 | 复现环境 VM=Windows，首版即跨平台验证；出问题 per-proxy 关 |

## 6. 实施步骤（分期，每期独立可交付可回退）

**Phase 0 — transport 内核（纯本地，无网络改动）**
1. `frame/quic_transport.go`：`NewQuicTransport(quic.Connection) FrameTransport`——control stream 收发 + datagram 收发 + 分片重组 + `Recv` 控制帧优先。
2. 本地 loopback 单测：两进程内存建 QUIC 连接，验证 FrameTransport 契约（含丢帧/乱序容忍、分片边界、Close 语义）。

**Phase 1 — 端到端升级通道（客户端 + Trojan/SOCKS5 服务端）**
3. 信令协议：新控制帧 `0x16 QUICUpgrade`（现有控制帧段 0x10-0x1F 内未占用值，需核实最新帧表）承载 {token, udpPort, certFingerprint}。
4. 客户端：`DialP2P` 返回升级为组合 transport（QUIC 主腿 + TCP 信令 fallback 探测）；失败回退现有 `NewStreamTransport`。
5. 服务端：Trojan/SOCKS5 同端口 UDP 监听 + token 匹配 + `p2p.HandleP2PTransport`。
6. 配置开关 `p2p_udp` + admin 会话列表显示传输形态（复用 connID/出口形态辨识既有展示位）。

**Phase 2 — h_tunnel 路径 + 实测**
7. h_tunnel direct mode 的 QUIC 升级（UDP 端口经信令腿协商）。
8. VM↔QG 实链路启用跑 iperf3 基线对比（TCP vs UDP 腿吞吐/RTT），数据入 docs/issues/ 或 runbooks。
9. 跨平台验证（Windows wintun + Linux tun）。

**Phase 3（后续可选，不在首版范围）**
- 内层 UDP 流量（DNS/QUIC 穿越）经 QUIC datagram 的真·UDP 端到端语义。
- NAT 穿透（直接 P2P QUIC，绕开中继）。

## 7. 验证标准

- 契约层：FrameTransport 全套单测过（含丢/乱/重、分片、控制帧优先、Close）。
- 回归层：不开 `p2p_udp` 时全部行为与现状 bit 级一致（零 diff 验证：现有 e2e 套件原样通过）。
- 功能层：开 `p2p_udp` 后 VM↔QG mesh 数据面正常（域名路由 + VIP + IPIP），admin 会话显示 QUIC 形态。
- 回退层：服务器侧关 `p2p_udp`，客户端开着也能正常建链（协商失败回退 TCP）。

## 8. 未决问题

1. `0x16` 帧值占用核实（Phase 1 第 3 步前 grep 全帧表）。
2. QUIC 腿的链路质量（SRTT/loss）是否纳入最优路径计算的 ping/pong 数据——首版**不纳入**（沿用 TCP 信令腿的 ping/pong），避免一处变更扩散到路由。
3. 服务器自签证书轮换策略（进程重启重新生成即可，重启后所有 QUIC 腿本就要重建，无需持久化）。
