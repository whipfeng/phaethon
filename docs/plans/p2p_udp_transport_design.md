# P2P 数据面 UDP 腿设计（链路级协商 + SOCKS5 首版）

**状态**: DRAFT（v3：用户定稿——链路级能力宣告、gossip 协商端口、0.0.0.0:P 寻址、TCP 兜底）
**创建**: 2026-11-03
**目标版本**: v1.0.0（首版仅 SOCKS5）
**关联文档**: [p2p_mesh_data_frame_loss_design.md](p2p_mesh_data_frame_loss_design.md)、[htunnel_p2p_direct_mode.md](htunnel_p2p_direct_mode.md)

## 1. 设计总则

- **UDP 腿是既有 TCP P2P 链路的可选叠加数据腿，松耦合**：TCP 腿永存且唯一承载 gossip（广播+协商+会话引导）；UDP 腿只承载 LinkPing/LinkPong/FrameMeshPacket，可随时整体回落 TCP，会话不中断、不死亡
- **链路级粒度**：能力宣告、端口分配、腿状态全部按"一条 TCP 会话（一个代理腿）"独立进行，与节点级无关
- **不新增帧类型**：协商借 gossip JSON 加两个字段；数据面帧格式不变
- **首版仅 SOCKS5**，但配置与机制对未来协议可扩展（§5）

## 2. 现状关键事实

- `FrameTransport` datagram 语义（`frame/transport.go:20-32`）：允许丢/乱/重，UDP 丢包契约天然满足，消费方零改动
- 帧格式 `[type:1B][len:2B][payload]`（gossip 0x12 / ping 0x14 / pong 0x15 / mesh 0x20）
- 客户端 `DialPacket()` 全类型已有（`dialer/dialer.go:86`）；SOCKS5 = `Socks5UDPAssociate`（`dialer/socks5_udp.go:231`），返回通用中继 PacketConn，每次 `WriteTo(payload, dst)` 带标准 SOCKS5 UDP 封头
- 服务端收包点：SOCKS5 `socks5UDPRelay.run()`（`server/socks5.go:342`）——datagram 在此解析目标并决定转发，**UDP 腿交付分支就加在这里**
- 会话入口：客户端 `P2PDialer.DialP2P()`（`dialer/socks5.go:81`）；服务端 `p2p.HandleP2PTransport(t, address)`（`p2p/p2p.go:2336`）
- gossip 载荷当前按节点构建、扇出到所有会话同一份——**链路级字段需要按会话构建载荷**（仅协商涉及方付出代价）

## 3. 协商与数据通路

**角色**：一条 TCP 会话的拨号侧 = 主动端，监听侧 = 被动端（每条链路独立协商）。

```
主动端                                                     被动端
─────                                                     ─────
gossip(udp:true) ──TCP────────────────────────────────────► 见宣告：
                                                            分配端口 P（每链路独立），
                                                            登记 P→会话逻辑监听
                ◄────────────────────gossip(udpPort:P)───  回带端口
DialPacket() 建标准 UDP ASSOCIATE（透明，零改动）
WriteTo(frame, 0.0.0.0:P) ──UDP通道──► 收包分支：dst=0.0.0.0:P
                                        命中登记 → 帧入会话
                ◄──UDP回包(src=0.0.0.0:P)── 条件满足才走 UDP 回：
                                        gossip 仍有宣告 且 收到过对方 UDP
```

- **datagram 目标 `0.0.0.0:P`**：unspecified 地址不可能与真实互联网目标冲突；SOCKS5 回包封头 src 原样回显目标（`server/socks5.go:593` 现有机制），主动端可校验 src 端口 == P
- **载荷** = 现有帧格式一字节不变；单包上限 1400B，超限两段分片、2s 重组超时丢弃（丢帧契约内）
- **回路径准入（被动端）**：必须同时满足 ①该链路 gossip 仍有 `udp:true` ②确实收到过该链路 UDP 消息——才记住对端返回地址并选择 UDP 回；否则一律 TCP

## 4. 状态机与 fallback（设计核心）

每链路独立状态机：

```
                配置开 p2p_udp（本端）
   无腿 ────────────────────────────► 宣告中( gossip 带 udp:true )
     ▲                                    │
     │                              DialPacket 成功
     │                                    ▼
     │                              有通道 ──gossip 收到 udpPort:P──► 已协商
     │                                    │                            │ 发送数据 dst=0.0.0.0:P
     │                                    │                            ▼
     │                                    │                         待激活
     │                                    │                            │ 收到首个 UDP 回包
     │                                    │                            ▼
     └────── K秒无UDP回流◄──── 激活 ◄──收到UDP且条件满足────── 被动端
              (TCP gossip 正常)              │
              撤回宣告,数据回TCP                │ UDP 腿静默死亡(同左 K 秒判定)
              退避 R 秒后重试 ─────────────────┘
```

fallback 规则：
1. **会话生死永不依赖 UDP 腿**：gossip 恒走 TCP，UDP 腿任何异常最坏结果是回到今天的纯 TCP 行为
2. **建不起来**（DialPacket/ASSOCIATE 失败）：保持宣告，退避重试（R=60s）；对端永远走 TCP，无感
3. **单向不通**（能发收不到回，如入向 UDP 被封）：主动端"待激活"状态 K=10s 内收不到任何 UDP 回包而 TCP 正常 → 撤回宣告、数据回 TCP、退避重试；被动端因条件②不满足从未走 UDP，无感
4. **中途静默死亡**（运营商限速断流）：同 3 判定（激活态 K 秒无 UDP 回流）
5. **对端关开关**：宣告字段从 gossip 消失 → 本端解除监听、数据回 TCP（被动端可选立即解除，在途帧丢失可容忍）
6. **回落期间 ping/pong 自动回 TCP**（与数据帧同路由函数），链路质量测量不中断；UDP 激活时 ping/pong 走 UDP（测的就是数据真实通路）

## 5. 配置与扩展性

```json
{ "name": "qg", "type": "socks5", "p2p": true, "p2p_udp": true }
```

- `p2p_udp` 挂 Proxy 配置（链路级），缺省 false = 与今天 bit 级一致
- 首版：非 SOCKS5 类型返回"unsupported"，等同关闭（info 日志）
- **未来协议扩展点**（现在不实现，接口先定好）：
  - 客户端：只要该类型的 `DialPacket()` 是真 UDP 线上 + `WriteTo` 可携带自定义目标，即可复用整套机制
  - 服务端：各服务器收包处实现同一交付分支（模式：解析 datagram → dst 为 unspecified → 查 p2p 端口登记 → 命中交付/未命中走原路径）。Trojan/h_tunnel 可加但无性能收益（其 UDP 线上仍是 TCP），故首版不做
  - Shadowsocks 等外部服务器（非 phaethon 进程）无法本地交付，永不支持

## 6. 改动面

| 层 | 位置 | 内容 |
|----|------|------|
| 配置 | `config/config.go` | `Proxy.P2PUDP *bool` + IsP2PUDP() |
| p2p 层 | `p2p/p2p.go`（会话/peer 结构） | 每会话 UDP 腿对象：状态机、端口登记、收发路由、gossip 字段注入（按会话构建载荷）、K/R 定时器 |
| 帧层 | `frame/packet_leg.go`（新） | datagram⇄帧 编解码 + 分片重组（纯函数，纯本地单测） |
| 客户端 | `dialer/socks5.go` `DialP2P` | `p2p_udp` 分支：DialPacket → 包 UDP 腿；失败走现有 TCP（行为不变） |
| 服务端 | `server/socks5.go` `socks5UDPRelay.run()` | 解析后加分支：`dst.IP.IsUnspecified()` → 查 p2p 端口登记 → 命中交付，未命中走原路径 |
| admin | 会话列表 | 会话行显示 udp/tcp 传输形态徽标 |

## 7. 实施步骤

- **Phase 0**：`frame/packet_leg.go` 编解码 + 分片重组 + 状态机纯逻辑单测（不碰网络）
- **Phase 1**：p2p 层腿管理 + gossip 字段 + SOCKS5 客户端/服务端分支 + 配置开关
- **Phase 2**：VM↔QG 实链路验证（含关 UDP 防火墙模拟 fallback）+ iperf3 TCP/UDP 腿吞吐对比基线入 docs/issues/
- **Phase 3**（未来）：其它真 UDP 协议接入（Shadowsocks/Hysteria2）

## 8. 验证标准

- `p2p_udp` 关闭/未配置：全部行为与现状 bit 级一致（现有 e2e 原样通过）
- 开启：VM↔QG mesh 数据面正常，admin 会话显示 udp 形态
- fallback：阻断 UDP 后自动纯 TCP 运行、业务无感；恢复后自动重新协商
- 不侵入阻塞调用、不拿业务锁（与 StallMonitor 兼容）；无 TUN 绕排他性

## 9. 未决问题

1. gossip 字段最终命名（`udp`/`udpPort` 或 `ulp`/`ulpPort`），Phase 1 前与现有 GossipInfo 字段风格统一
2. K=10s / R=60s / 1400B 为初值，实测后调
3. 双会话（同对端两条链路）各自独立协商端口，互不干扰——实现时确认按会话构建载荷的开销可忽略
