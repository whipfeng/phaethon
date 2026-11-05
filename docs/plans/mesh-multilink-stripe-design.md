# mesh 数据面多链路散流设计（同 proxy 动态 stripe + 加权散流）

| | |
|---|---|
| 版本 | v0.1.0 |
| 状态 | DRAFT（2026-11-05 讨论定稿，待用户批准） |
| 关联 | [p2p_udp_transport_design.md](p2p_udp_transport_design.md)（UDP 腿，本设计的最高优先形态）、[mesh-datapath-perf-design.md](mesh-datapath-perf-design.md)（前三阶段调参）、[mesh-large-file-slow.md](../issues/mesh-large-file-slow.md) §9（架构研判）、[htunnel_p2p_direct_mode.md](htunnel_p2p_direct_mode.md)（动态多 lane 范本） |

## 一、背景与结论链

前三阶段调参（TCP 栈参数/可观测性/DRR+writev）把 Mac→VM 大文件中位提速 1.6x、drop 归零，但**吞吐分钟级波动 + 偶发长卡死被 A/B 实验证明是存量问题**：外层单条 TCP 有序字节流的 HoL + relay 路径 TCP-over-TCP meltdown（issue §9.2），调参治不了协议形态。

演进方向讨论的三个关键结论（用户拍板）：

1. **隧道传 IP 包、允许丢**：内层 TCP 自带可靠性，隧道层不叠 ARQ/stream 层。QUIC 类方案否决（重复分层）；直连 UDP 监听方案否决（现有 proxy 腿已具备 UDP 能力，不新增监听面）。
2. **传输形态三级优先级**：UDP 腿（已有机制，`udp_leg_transport.go`，点亮即用）> 多链路散流 > 单条 TCP。datagram 语义的传输（UDP 腿、htunnel v2 的 POST/GET 池）天然免疫 HoL，不叠散流。
3. **多链路的两种来源**：不同 proxy 的多链路 = 用户配置域（`proxyPeers` 按 proxy.Name dedup，每个 proxy 一条并行连接，已存在）；**相同 proxy 的多链路 = 动态适应，本设计要新加的工作**。

路由层不动：Dijkstra 已按链路粒度选路（`TopologyGraph.Edges[from][to] = []LinkQualityInfo` 多链路并行边，`dijkstraResult.LinkNIC` 选定出口链路，`mesh.go:3498/3240`）。

## 二、目标与非目标

**目标**：
- 同 proxy 下动态建立/拆除多条并行数据腿（stripe），HoL 爆炸半径从 100% 降到 1/N
- 出向散流覆盖"配置的多 proxy 连接 ∪ 动态 stripe"并集，按质量加权、按流粘性
- 与 UDP 腿正确互斥：UDP leg Active 时数据面走 UDP，stripe 收敛

**非目标（明确不做）**：
- QUIC / 可靠性隧道层 / ARQ / 应用层分片
- 直连 UDP 监听（节点级 `p2p_udp_listen`）
- 跨 stripe 重排序、MPTCP 式带宽聚合调度（散流为隔离不为提速）
- stripe 走不同 proxy（路径配置是用户域；stripe 只在同一 proxy 链下倍增）
- 路由层（Dijkstra/gossip/linkCost）任何改动

## 三、现状关键事实（代码锚点）

| 事实 | 锚点 |
|---|---|
| P2P 会话 = 1 transport + 单写者 peerWriteLoop | `p2p/p2p.go:573` |
| 写路径：enqueueWriteReq → 优先队列 → DRR → SendBatch | `p2p/p2p.go:824`、NextBatch/DRRScheduler |
| hashFlow（内层 5-tuple Jenkins+salt） | `p2p/p2p.go:984` |
| proxyPeers 按 proxy.Name dedup（active 侧每 proxy 一条连接） | `p2p/p2p.go:87` |
| FrameTransport datagram 语义（丢/乱/重容忍） | `frame/transport.go:20-32` |
| UDP 腿 = session 内叠加数据腿，FSM 常驻 + 随时回落 TCP | `p2p/udp_leg_transport.go`、`docs/plans/p2p_udp_transport_design.md` |
| 帧类型：gossip 0x12 / ping 0x14 / pong 0x15 / mesh 0x20 | UDP 腿设计文档 §2 |
| 被动端会话入口 `HandleP2PTransport`、主动端 `P2PDialer.DialP2P` | `p2p/p2p.go:2336`、`dialer/socks5.go:81` |
| 每条连接独立 LinkNIC、路由按链路选出口 | `mesh/link_nic.go`、`mesh.go:3498` |
| htunnel v2 动态多 lane 范本（GET 伸缩/POST 池） | `dialer/htunnel_direct.go:50` |

## 四、总体架构

```
                       ┌─ UDP leg Active？─── 数据面全走 UDP 腿（无 HoL，stripe 收敛）
                       │
peer 出向（FrameMeshPacket）─┤─ stripe 集合 = 跨 proxy 连接（配置） ∪ 同 proxy stripes（动态）
                       │    └ 流粘性哈希（rendezvous，按质量加权）→ 选 1 条
                       └─ 控制面（gossip/hello/heartbeat/DNS）恒走 primary 连接，不散
```

## 五、同 proxy 动态 stripe 设计

### 5.1 定位：session 内数据腿，不是第二条会话

stripe 形态对齐 UDP 腿：**复用现有会话的身份/认证/gossip，只加一条裸数据通路**。不建第二条完整 P2P 会话——避开 `proxyPeers` dedup 冲突、gossip 翻倍、会话协议改动面。代价是新增一个轻量握手帧。

### 5.2 建立：StripeAttach 握手

- 新帧类型 `0x16 StripeAttach`，payload = `{sessionID, stripeID}`，仅作为 stripe 连接首帧，永不上 UDP 腿
- 主动端：同一 proxy 重复 `DialP2P`（复用现有 dialer 链），首帧发 StripeAttach 绑定已有 session
- 被动端：`HandleP2PTransport` 收到 StripeAttach → 按 sessionID 查现存会话 → 命中则登记为 extra data leg（复用 UDP 腿的端口登记/会话归属机制）；未命中关闭
- 未做 attach 的连接走现有会话握手（现状行为零变化）

### 5.3 写路径改造

```
enqueueWriteReq（不变，预算/DRR 记账不变）
  → DRR NextBatch（不变，流间公平）
  → stripe 选择：hashFlow 同 key → rendezvous hash(stripe 集合, 质量权重) → 流亲和表（带空闲过期）
  → per-stripe 发送队列 → per-stripe writeLoop（独立 writev/SendBatch）
```

- 单流保序由粘性保证；DRR 仍管共享 stripe 上的流间公平
- 数据面仅 `FrameMeshPacket` 散；DNS/控制帧恒走 primary（现有优先队列语义不动）
- 每 stripe 独立 8MB 预算子账或共享 peer 预算按 stripe 配额分（实现期定，倾向共享+per-stripe 水线防饿死）

### 5.4 动态控制器

| 信号 | 阈值初值 | 动作 |
|---|---|---|
| DRR 出队积压持续（budget 消耗速率/队列占用） | 持续 30s 超 70% | 加 1 条 stripe（慢加，逐条，间隔 ≥10s） |
| per-stripe LinkPing srtt/丢包 | 已有 pingTracker 扩展按 stripe 测 | 健康度输入 + 散流权重 |
| 积压清空 | 持续 5min | 减 1 条 stripe（慢减） |
| 硬上限 | 4（先验值，实测校正） | 不再加 |
| UDP leg 进入 Active | FSM 回调 | 不再加；存量 stripe 自然到期回收 |
| UDP leg 进入 Backoff | FSM 回调 | 恢复加线逻辑 |

hysteresis 防 stripe 数量自身 flapping；加线代价 = proxy CONNECT（trojan +TLS）1-2 RTT，故慢加。

### 5.5 接收端

per-stripe readLoop 读帧 → 仅接受 `FrameMeshPacket/LinkPing/LinkPong` → 直通现有 mesh 入向路径（与 UDP 腿 dataCh 同一消费方）。gossip/控制帧出现在 stripe 上视为协议错误，记 warn 丢弃。

### 5.6 生命周期

- primary 会话断开 → 全部 stripe 关闭（数据腿依附会话）
- stripe 单条死亡（写错误/ping 超时）→ 控制器标记下线，亲和表失效该 stripe 的流（rendezvous 天然最小重映射），不重建直至加线条件再满足
- 重连后 sessionID 变化，旧 stripe 自然失效

### 5.7 与 UDP 腿的互斥

控制器订阅 UDP leg FSM 状态（`udp_leg_transport.go` 现有 phase 回调）：Active 期间数据面已由 UDP 承载，加线无意义且浪费握手；Backoff/回落 TCP 后恢复。两者共享同一"数据腿"抽象位（session 的 extra data legs 集合），观测上可统一展示。

## 六、散流决策：流亲和 + 质量加权

- **流 key**：hashFlow 现有 5-tuple hash（复用，零新增）
- **算法**：rendezvous hashing（最高随机权重）——stripe 集合增减时最小化流重映射，天然适配动态上下线
- **权重**：每条候选连接（跨 proxy 连接 + 同 proxy stripe）本地 ping 统计（srtt/loss → 分母罚没，参考 linkCost 形态）。出向决策在本节点做，**只看本节点到下一跳各连接的质量，不依赖 gossip 权重**
- **亲和表**：flowHash → 连接指针，空闲过期（如 60s），表项含最近一次权重快照，权重劣化超阈时主动失效重哈希

## 七、观测性

- admin badge 扩展：`udp` / `tcp+stripe×N` / `tcp`
- per-stripe 计数：发送字节/帧、srtt/loss、当前散流权重、亲和表大小
- 控制器事件日志：加线/减线原因（信号原文）
- 复用现有 connID 贯通机制（ff4d50f）标注 stripe 归属

## 八、分期实施与验证判据

**阶段 0（运营，零代码）**：普查（链路 × UDP 就绪矩阵）→ QG/Mac 开 `p2p_udp` 点亮 UDP 腿 → trojan 腿 UDP relay 可用性验证。判据：admin badge 变 `udp`、50MB scp 复测对比 v301 基线（Mac→VM 中位 69s）。**VM 留跑取证期不动配置**。

**阶段 1（骨架）**：StripeAttach 帧 + per-stripe 写读 + 固定 1 stripe。判据：全部现有单测/e2e 通过，行为与现状零差异。

**阶段 2（动态控制器）**：信号采集 + 上下线。判据：压测下 stripe 数随积压上升/回落，日志原因可解释。

**阶段 3（散流决策）**：rendezvous 加权 + 亲和表 + 跨 proxy 并集。判据：多 proxy 配置环境双链路利用率接近权重比；kill 单 stripe 后流平滑迁移不中断。

**阶段 4（验证）**：Mac↔VM 50MB scp 三取向对比、长稳 24h、故障注入。判据：HoL 注入（单 stripe 延迟劣化）下吞吐损失 ≤1/N。

## 九、风险与开放问题

| 风险/开放问题 | 处置 |
|---|---|
| UDP 腿点亮后 >1500B 走外层 IP 分片，分片 all-or-nothing | v3.2 已定不钳 MTU；阶段 0 实测丢包率，超标再议 |
| trojan 服务端 UDP relay 实际可用性未验证 | 阶段 0 验证项 |
| stripe cap=4、积压阈值均为先验值 | 阶段 4 用实测校正，全部常量化 |
| per-stripe 预算分账方案（共享 vs 配额） | 阶段 1 实现时定 |
| UDP 被中间盒限速导致腿间抖动 | FSM 回落兜底；观测送达率 |
| 留跑取证期 stall dump（dvip 冻结）与本设计的关联 | 独立线（mesh-data-plane-stall.md 分叉判据），不阻塞本设计 |
