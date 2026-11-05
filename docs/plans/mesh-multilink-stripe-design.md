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

### 4.1 传输形态三级优先级

```
                       ┌─ UDP leg Active？─── 数据面全走 UDP 腿（无 HoL，stripe 收敛）
                       │
peer 出向（FrameMeshPacket）─┤─ stripe 候选集 = 本会话（单 proxy）内的 lanes：
                       │    primary ∪ 动态扩容（rendezvous 按质量加权，§4.3 永不跨 proxy）
                       └─ 控制面（gossip/hello/heartbeat/DNS）恒走 primary 连接，不散
```

### 4.2 核心决策：stripe 对路由呈现**单一链路**，封装在传输层（2026-11-05 用户决策点定案）

同 proxy 多链路封装为 `FrameTransport` 包装器（形态对齐 `udp_leg_transport` / ht v2 连接池），对上层（P2P 会话/gossip/Dijkstra/admin 链路模型）**完全呈现为现有的单链路**，理由：

1. **路由稳定性**：stripe 动态上下线。若每条 stripe 成为独立 GossipLink，Dijkstra 拓扑随 stripe 增减抖动 = 控制面 flapping，比数据面 HoL 更难治。动态性必须藏在静态接口后面。
2. **语义正确性**：stripes 是同一路径的倍数，不构成不同链路。链路质量（SRTT/loss/jitter/sojourn）描述路径特征，4 条 stripe 质量高度相关；汇报为多条独立链路会制造虚假的路径多样性，路由可能误判"互为备份"。
3. **先例一致**：ht v2 的 POST 池/动态 GET 对会话呈现单 transport；UDP 腿同理。传输层封装是项目既有模式。
4. **实现简化**：gossip 零新字段、Dijkstra/GossipNeighbor/dijkstraResult 零改动、proxyPeers 模型不动、**DRR/peerWriteLoop/enqueueWriteReq 全部不动**——散流不在写路径上发生，而在 transport SendBatch 内发生。改动全部收进包装器。

**质量上报语义**：GossipLink 指标 = 该 proxy 链路聚合质量（LinkPing 恒走 primary lane，对外行为与现状一致）；per-stripe 质量只在包装器内部使用（散流权重 + 控制器），不上 gossip。

### 4.3 决策心路：容量问题归 stripe 动态扩容，proxy 间是异构 LinkNIC 域（2026-11-05 用户定案）

**心路**：散流候选集最初设计为"跨 proxy 连接 ∪ 同 proxy 动态 stripe"并集 → 推演发现跨 proxy 池化需要会话耦合 + 传输/路由双控制环 + 越界路径选择 → 且路由 cost 已含 sojourn/dropRate（`mesh.go:3402`），拥塞自纠环（打满 → 排队时延/丢包涨 → cost 涨 → Dijkstra 切路）已存在 → 进一步质疑"单链路打满溢出空闲 proxy"是否算损失 → **用户定案：不算，也不该做**。

**结论**：不同 proxy 本来就是不同 LinkNIC 隔离域，链路属性（SRTT/loss/jitter/sojourn）可能完全不同；节点间多 proxy 是**冗余互备红利，不是容量池**。

- 链路容量问题 → **stripe 动态扩容**解决（同属性域内、亚秒级、4 条 cwnd 在同瓶颈竞争拿满路径份额）
- 跨域切换 → **路由职责**（质量驱动、秒级、gossip 已含负载信号、hysteresis 防振荡）
- stripe 候选集 = **单 proxy 会话内的 lanes**，永不跨 proxy、不做跨 proxy 聚合

## 五、同 proxy 动态 stripe 设计

### 5.1 定位：session 内数据腿，不是第二条会话；传输层封装，路由视为单链路

stripe 形态对齐 UDP 腿：**复用现有会话的身份/认证/gossip，只加一条裸数据通路**。不建第二条完整 P2P 会话——避开 `proxyPeers` dedup 冲突、gossip 翻倍、会话协议改动面。对外（§4.2）呈现单一 FrameTransport。代价是新增一个轻量握手帧。

### 5.2 建立：StripeAttach 握手

- 新帧类型 `0x16 StripeAttach`，payload = `{sessionID, stripeID}`，仅作为 stripe 连接首帧，永不上 UDP 腿
- 主动端：同一 proxy 重复 `DialP2P`（复用现有 dialer 链），首帧发 StripeAttach 绑定已有 session
- 被动端：`HandleP2PTransport` 收到 StripeAttach → 按 sessionID 查现存会话 → 命中则登记为 extra data leg（复用 UDP 腿的端口登记/会话归属机制）；未命中关闭
- 未做 attach 的连接走现有会话握手（现状行为零变化）

### 5.3 写路径：包装器内分流，peer 写路径不动

```
peerWriteLoop（不动）
  → DRR NextBatch（不动，流间公平在 batch 内已排好）
  → stripeTransport.SendBatch(frames)：
       frames 按内层 5-tuple hash 分组（流粘性，同 hashFlow 语义）
       rendezvous 加权选 lane（§六）
       每 lane 独立 writev 发送
```

- **enqueueWriteReq / DRR / 预算记账全部零改动**——散流发生在 transport 内部，这是 §4.2 封装决策的最大红利
- 数据面仅 `FrameMeshPacket` 参与分流；wrapper 识别控制/DNS 帧一律走 primary lane
- 每 lane 独立写循环与写缓冲；lane 间不做任何重排序（帧 datagram 语义）

### 5.4 动态控制器

| 信号 | 阈值初值 | 动作 |
|---|---|---|
| 包装器发送队列积压持续 | 持续 30s 超 70% | 加 1 条 lane（慢加，逐条，间隔 ≥10s） |
| per-lane 内部探测 srtt/丢包 | lane 内 LinkPing/LinkPong | 健康度输入 + 散流权重 |
| 积压清空 | 持续 5min | 减 1 条 lane（慢减） |
| 硬上限 | 4（先验值，实测校正） | 不再加 |
| UDP leg 进入 Active | FSM 回调 | 不再加；存量 lane 自然到期回收 |
| UDP leg 进入 Backoff | FSM 回调 | 恢复加线逻辑 |

hysteresis 防 lane 数自身 flapping；加线代价 = proxy CONNECT（trojan +TLS）1-2 RTT，故慢加。

### 5.5 接收端：多 lane fan-in（udp_leg 先例）

per-lane readLoop 读帧 → 仅接受 `FrameMeshPacket/LinkPing/LinkPong` → 汇入单一 Recv 通道（同 `udp_leg_transport` 的 dataCh/ctrlCh 模式），会话层 readLoop 零改动。gossip/控制帧出现在 lane 上视为协议错误，记 warn 丢弃。对端出向怎么散是本端不可见也不关心的（双向各自封装）。

### 5.6 生命周期

- primary 会话断开 → 全部 lane 关闭（数据腿依附会话）
- lane 单条死亡（写错误/内部探测超时）→ 控制器标记下线，亲和集合失效（rendezvous 天然最小重映射），不重建直至加线条件再满足
- 重连后 sessionID 变化，旧 lane 自然失效

### 5.7 与 UDP 腿的互斥

控制器订阅 UDP leg FSM 状态（`udp_leg_transport.go` 现有 phase 回调）：Active 期间数据面已由 UDP 承载，加线无意义且浪费握手；Backoff/回落 TCP 后恢复。两者同为 session 的"传输层叠加数据通路"，封装位一致，观测统一展示。

## 六、散流决策：流亲和 + 质量加权（包装器内部）

- **流 key**：内层 5-tuple hash（与 `hashFlow` 同语义，包装器自带 salt，独立于 DRR 的 salt）
- **算法**：rendezvous hashing（最高随机权重）——lane 集合增减时最小化流重映射，天然适配动态上下线
- **权重**：每条 lane 的内部探测质量（srtt/loss → 分母罚没，参考 linkCost 形态）。出向决策在本节点包装器内做，**只看本地质量，不依赖 gossip 权重**（§4.2：gossip 只承载聚合后的链路质量）
- **控制/DNS 帧不参与分流**（恒走 primary lane）；`FrameMeshPacket` 才分流
- 权重劣化超阈 → 该 lane 的亲和流主动失效重哈希

## 七、观测性

- admin badge 扩展：`udp` / `tcp+stripe×N` / `tcp`
- per-stripe 计数：发送字节/帧、srtt/loss、当前散流权重、亲和表大小
- 控制器事件日志：加线/减线原因（信号原文）
- 复用现有 connID 贯通机制（ff4d50f）标注 stripe 归属

## 八、分期实施与验证判据

**阶段 0（运营，零代码）**：普查（链路 × UDP 就绪矩阵）→ QG/Mac 开 `p2p_udp` 点亮 UDP 腿 → trojan 腿 UDP relay 可用性验证。判据：admin badge 变 `udp`、50MB scp 复测对比 v301 基线（Mac→VM 中位 69s）。**VM 留跑取证期不动配置**。

**阶段 1（骨架）**：`stripeTransport` 包装器（FrameTransport 接口，形态对齐 udp_leg_transport）+ StripeAttach 握手 + per-lane 写读 + fan-in。判据：全部现有单测/e2e 通过，行为与现状零差异；peer 写路径（enqueue/DRR/预算）零改动。

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
