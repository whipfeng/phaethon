# Mesh 链路：控制面隔离 + 在途量上限

**状态**: APPROVED（2026-11-07 评审通过，含 §2.6 双向 lane 控制修订）
**创建**: 2026-11-07
**问题来源**: `docs/issues/vm-gg-ssh-proxy-link.md`
**相关方案**: `mesh-multilink-stripe-design.md`（stripe lane）、`htunnel_p2p_direct_mode.md`（HT v2）

## 1. 背景

VM→GG（trojan via SSH 跳板）压测证据：
- VM 进程内队列 <256KB、SSH keepalive <1s、GG Recv-Q=0，瓶颈在跳板→GG 段（≈46KB/s，丢包重）
- VM 与 GG 之间压住 ≈2MB+（sshd channel 窗口 + 跳板内核缓冲），全在我们进程外面
- 同一条连接上的 ping/DNS/ACK 跟着排几十秒 → `link dead held`、DNS 5s 超时、链路被判死重建

问题本质是两个：
1. **控制面和大流量挤同一条连接**（控制帧排在别人的缓冲后面，进程内优先级无效）
2. **进程外积压不受控**（内层 TCP 只看到 RTT 涨到几十秒，不丢包，不会降速，反而超时重传浪费带宽）

## 2. A：控制面隔离（专用数据 lane）

### 2.1 规则
- 满足触发条件的链路，主动方会话建立后**立即**拨一条**专用数据 lane**（复用现有 `dialStripeLane` + StripeAttach，被动方无需改协议）
- 专用 lane 存在时：所有可拆分数据帧（`splittable` = 非 DNS 的 FrameMeshPacket）**只走数据 lane**，主连接只走控制帧、DNS、ping/pong
- 动态 lane（拥塞控制器加的）照旧参与分流，但主连接不再参与分流（rendezvous 候选集去掉 primary）

### 2.2 触发条件（自动，无配置）
主连接的底层 carrier **没能设上 TCP_NOTSENT_LOWAT**（`frame.limitKernelBacklog` 走到 skipped 分支）：
- 经代理/中继的 carrier（SSH channel、via 链、非 TCP socket）
- Windows 上所有链路（不支持 NOTSENT_LOWAT）

能设上的（Linux 直连 TCP）积压本来就留在进程内 DRR，控制帧已能优先，不开。
实现：`limitKernelBacklog` 返回 bool，dialer 侧把结果挂到 transport 上供 p2p 读取。

### 2.3 专用 lane 的生命周期
- 专用 lane **不受**降级剔除（`srtt≥1s / loss≥50%` 那条）和 5 分钟空闲回收约束
- 专用 lane 写/读失败被移除 → 数据回落主连接（现有行为），控制器 5s 后重拨；重拨失败指数退避（5s→60s 封顶）
- `laneEnqueue` 满等待 50ms 后改走主连接的回落：专用 lane **不回落**（回落就又混到一起了），改为由 §3 的在途门控在 DRR 前拦住

### 2.4 ping 语义
ping 仍按 Phase C 进数据 DRR 队列（测进程内数据面排队），出 DRR 后走主连接。
§3 把积压拉回进程内后，ping 的 SRTT 仍能反映数据面拥塞，路由代价计算不受影响。

### 2.5 不覆盖
HT v2（POST/GET 池）不走 stripe（p2p.go:1478 明确排除），由 §4 单独处理。

### 2.6 双向 lane 控制（2026-11-07 评审修订，替代原"仅主动端控制器"）

**修订动机**：TCP 全双工，被动端出站数据同样搭在主动端建的 lane 上（p2p.go:1127 被动端不传 dialLane、controllerLoop 不启动）。原设计下反向重载 + 路径不对称拥塞时，被动端只有 B2 门控的"停写"阀门、没有扩容杠杆——既然计量和门控都已是双向独立的，扩容决策也应该是双向的。

**关键约束：两端都能触发，但只有主动端执行拨号**。被动端自己拨号不可靠——会话方向本身就说明建会话时只有一方有到达对端的路由（QG 无公网 IP 即此类：GG 永远拨不进 QG）。所有 lane 仍由主动端建立，stripeID 仍由主动端单方分配（无需分区），StripeAttach 入向接受（p2p.go:1062-1073）、probe、降级剔除逻辑零改动。

**规则**（2026-11-07 二次修订：**扩缩决策统一在主动端，无 lane-request 帧**；对端状态以遥测上报，主动端单方裁决）：
- **扩**：主动端 controller 判据 = 本地 gauge 超阈 **或对端遥测超阈**，持续 30s、间隔 10s、上限 stripeMaxLanes=4 约束不变。被动端无请求帧、无 controllerLoop——它是纯发送方，只上报状态。
- **缩**：本地 gauge 低 **且 对端遥测低**，持续 5min → 主动端删最新 lane。
- **遥测传输（gossip 兜底 + FrameGauge 越阈紧急通告；2026-11-07 终审，原"LaneAck 捎带 1B"作废）**：gauge/gated-wait 作为 gossip 字段周期广播（仿 udp:true/udpPort 注入模式），上/下穿 0.7 水位时另发 FrameGauge（0x18）紧急通告。lane 扩缩裁决的判据窗口是 30s/5min 级，gossip 秒级周期足够；与 50ms 级流控通告频率差 2-3 个数量级，**不同目的不混帧**。
  - **scope 约束（实现红线）**：gossip 是全网 flood 的每节点字段，gauge 是 per-session 的——只携带"最拥塞会话摘要（gauge + 对端 nodeID）"或有界活跃会话小表，防止 gossip 体积随全网会话数膨胀
  - 对端在 gossip 中声明需要什么，本端即在 gossip 中周期返回对应兜底数据（沿用 udp:true/udpPort 的请求-响应模式）
- **gauge 信号源调整（B2 的前置约束）**：B2 门控把超额数据刻意留在进程内 DRR 队列 → sojourn 长期偏高 → 原 gauge 会"永远拥塞"。B2 实施时同步把 gauge 信号改为**门控等待时长占比**（或：门控已生效的 lane 冻结其加 lane 决策），不再用裸 sojourn。

**现状代码基线**（对账核实）：主动端 p2p.go:1482-1490 传 dialLane；被动端 p2p.go:1127-1130 不传；controllerLoop 仅 cfg.dialLane != nil 时启动（stripe_transport.go:184-185）；入向 StripeAttach 由 p2p.go:1062-1073 peek 首帧 + attachStripeLeg:1182 绑定，被动端接受逻辑零改动。

## 3. B：在途量上限（所有 TCP 类链路通用）

### 3.1 计量
- 发送方：每条 lane（主连接 laneID=0，其余用 stripeID）累计 `tx` = 成功交给 transport 写出的帧 payload 字节
- 接收方：每条 lane 累计 `rx` = 从该 lane 读到的帧 payload 字节
- **在途量** = `tx − ackedRx`，就是压在 carrier 缓冲、跳板、网络里的数据

### 3.2 信令架构（2026-11-07 终审：全量 gossip 兜底 + 双紧急通告帧，无协商帧）

**信令总原则**：**所有信息（能力、需求声明、tx 计量、gauge 遥测）都走 gossip 周期兜底通告；内容是否携带取决于对方在 gossip 中声明的"要不要"**——需求即声明，无需任何协商握手。紧急变更另用专职简单控制帧立即通告：**双专职帧一个通告 tx、一个通告 gauge 等，都是到达阈值后的紧急通告，不是协商帧**。

- **gossip 兜底（周期、全网、可迟到；仿 udp:true/udpPort 注入模式，mesh.go GossipFields）**：
  - 能力/需求声明：`lack:true`（支持 LaneAck）、`wantTx` / `wantGauge`（对端要什么，双向各自声明）
  - tx 计量：各 lane 最新累计 rx（仅当对端声明 wantTx）
  - gauge/gated-wait 遥测（仅当对端声明 wantGauge；scope 约束见 §2.6）
- **FrameLaneAck = 0x17（紧急 tx 通告）**：非空，重复 `{laneID uint32, rx uint64}`（12B/条，≤64 条）。本端 rx 距上次通告增量达到阈值（≥50ms 且有变化，或出现大增量）**立即发**，走主连接 control 优先级，不等 gossip 周期；gossip 兜底同内容
- **FrameGauge = 0x18（紧急 gauge 通告）**：gauge 上穿/下穿 0.7 水位时**立即发**；gossip 兜底同内容
- **无协商帧**：废除 B1 的空 payload 语义。接收方见到对端 gossip 声明 `wantTx` 即置位 ackRequested 开始回报——不靠请求帧触发，天然无时序耦合
- 新老混跑：老 peer 不声明 want、不认识两帧 → 不回报、发送方永不门控，行为同现状
- B1 的空 payload 语义在实施 B2 时废除（0x17 保持非空紧急通告，新增 0x18）；对老版本无额外兼容负担

### 3.3 上限（延迟驱动公式）

原方案 `cap=clamp(2×rate×max(baseRTT,50ms),64KB,4MB)` 有两个发散问题：
- baseRTT 在我们自己的队列膨胀时会虚高，导致 cap 越涨越大
- 纯速率上限在 RTT 超过目标时会崩溃

改用 **LEDBAT-like 延迟驱动公式**：
```
minRTT      = 过去 10 分钟每分钟的最小 RTT（滑动窗口）
qd          = max(rtt − minRTT, 0)    // 排队延迟，负值按 0 处理
target      = 200ms                  // 目标排队延迟

if qd > target:                      // 拥塞：减速
    cap *= max(0.5, target/qd)
elif qd < target/2 and busy:         // 空闲：加速（busy = inflight ≥ cap/2 自上次调整）
    cap *= 1.25

cap = clamp(cap, 64KB, 8MB)         // 边界
initial_cap = 256KB                  // 初始值
```

- **负 qd 防护**（2026-11-07 修订，B2 必须实现）：qd<0 一律按 0 处理且**不得触发 ×1.25 加速**。门控生效前链路 RTT 是几十秒的病理值，10 分钟 minRTT 桶全被污染；门控生效后 rtt 回落会让 qd 长期为负，若不钳制会判"空闲"→ cap 涨回 8MB → bufferbloat 复发。检测 qd 持续为负或桶龄异常时重置 minRTT 基线（从门控生效后的样本重建）。
- **停滞防护**：ack 无进展 10s 且 inflight > 0 → 解除门控并告警 `[LANE-STALLED]`。46KB/s 链路上 256KB cap 需 ~5.5s 排干，ack 仍在推进（rx 每 50ms 回报变化），不会误触发；真停滞 = ackedRx 冻结
- ×1.25 加速步长保守（同 LEDBAT off-target gain），避免震荡
- 数值示例修正（2026-11-07 对账）：VM→GG 链路 rtt≈55ms **就是 minRTT**，qd≈0，LEDBAT 公式不会主动降 cap——该链路的收益来自 **initial_cap=256KB** 把进程外积压从 2MB+（≈45s 排队）压到 256KB（≈5.5s）；快链路（10MB/s、20ms）qd≈0 时 cap 经 ×1.25 爬升至 8MB 上限，不限速

### 3.4 门控位置（关键）
在 `peerWriteLoop` 出队处，不在 transport 写入处：
- 数据可写额度不足时，`peerWriteLoop` **暂停从 drrOutput 取数据**，但继续取 controlCh / dnsQueue
- 额度 = 数据 lane 们的总在途 < 总 cap（有专用 lane 时只算数据 lane；否则算主连接）
- ack 到达 / 写完成时通知 `peerWriteLoop` 重新检查
- 效果：多出的数据留在 DRR 队列（公平调度、8MB 预算、满了丢包 → 内层 TCP 正常降速），控制帧永不被门控卡住

### 3.5 不覆盖
UDP 数据腿（不经 TCP 缓冲，自带机制）；HT v2 本期不接门控（§4 先解决控制面混排）。

## 4. C：HT v2 控制帧与数据帧分开

### 4.1 现状问题（Agent 审查结果）

**主动端（客户端 QG）**：
- `Send/SendBatch` 忽略 `isControl`，控制帧和数据塞同一个 POST body，排在 16 个 POST 名额后面
- `sendSem` 16 槽并发，新 pong/gossip 等所有 16 槽满时阻塞
- POST 超时 10s，失败静默丢失（仅 debug log），无重试
- GET `dataRecvCh`（64）满了会阻塞整个读循环，同一 GET body 后面的控制帧跟着等
- `io.ReadAll` 错误被忽略，512KB body 整体 AEAD 解密，慢链路下头帧延迟可达 ~20s（46KB/s）
- GET 超时基于 wait + 10s，慢 body 截断后解密失败直接拆会话

**被动端（服务端 JF）**：
- POST 入向 `meshIn` 单 FIFO（4096），`Recv` 无控制优先
- 16 个 POST handler 并发写入；`meshHandleWrite` 阻塞在 meshIn **无超时**（htunnel_mesh.go:135-140），慢 session 会占住 handler
- egress `meshOut`（4096）+ `meshCtrlOut`（256），但 peerWriteLoop 在 8MB gate 关闭时阻塞，pong 排队
- CoDel 几乎无效：每批最多丢一帧，sojourn 时间不含 meshOut 排队和传输时间
- DNS 走数据 FIFO，可被 CoDel 丢弃

**2026-11-07 对账修正**："无隔离通道"的说法对 egress 和客户端接收侧已过时——服务端 meshCtrlOut（§3.9 遗产）、客户端 ctrlRecvCh/dataRecvCh 分离 + isCtrlLoop 双 GET 循环（htunnel_direct.go:361/374/408）均已存在。**真实缺口只剩两处**：客户端 POST **发送**侧（Send 忽略 isControl，htunnel_direct.go:140）和服务端入站 **Recv** 侧（meshCtrlIn 不存在）。P1/P2 正是补这两个缺口，不做四套隔离。

**根因**：
1. **GET 队头阻塞**：512KB body 整体解密，头帧（如 pong）不可见直到整个 body 到达
2. **POST 重排序**：16 槽并发 + handler 交错写入
3. **控制帧混排**：无隔离通道，pong/DNS 排在大数据后面
4. **缓冲失控**：~1.5MB（POST）+ 6MB（GET）+ 8MB（gate）进程外积压

### 4.2 修复方案（按优先级）

#### P1：客户端控制 POST（立即可做，兼容旧服务端）
- `isControl=true` 或 batch 中有 Control 帧 → 单独小 POST
- 专用信号量 1–2 槽（不占 `sendSem` 16 槽）
- 超时 5s；**连续 N 次（建议 3）失败才上报 session 触发链路重建**（2026-11-07 修订：现有代码 POST 失败与链路死活无耦合、靠 ping 独立保活，单次失败即重建会把服务端抖动放大成链路抖动）
- wire format 不变（3B header + payload，整体 XChaCha20）

#### P2：服务端入站分流（server-only，兼容旧客户端）
- `meshHandleWrite` 中路由非 FrameMeshPacket 和 DNS 到 `meshCtrlIn`
- `Recv` 先服务 `meshCtrlIn`，再服务 `meshIn`
- handler 阻塞时限（如 2s），超时丢弃并告警

#### P3：GET 控制隔离
- **Server-only 第一步**：ctrlQ 非空时批次限制为控制 + ~16KB 数据
- **后续**：专用控制 GET，新请求头标记；旧服务端忽略头，保持现状；新服务端仅在客户端发过头时才切换

#### P4：客户端 recv 修复
- 检查 `ReadAll` 错误，非 200 重试 transient 5xx（指数退避）
- 基于 body 进度设超时（而非 wait 时间）
- 解析完整个 body 后先交付控制帧，再交付数据

#### P5：按字节限大小
- GET 批次自适应：`clamp(rate × 200ms, 16KB, 512KB)`
- POST 按在途字节限流，减少并发槽数（2–4 大槽代替 16 小槽）降重排序

#### P6：DNS 优先级（服务端）
- `meshChannelTransport.Send` 中用 `isDNSPacket` 分类
- 新增 DNS 队列，在控制之后、数据之前服务
- 不被 CoDel 丢弃

#### P7：HT v2 作为 laneID 0 纳入 LaneAck 门控
- **适用范围（2026-11-07 确认）**：LaneAck 的请求/回报/计量/门控整体适用于 HT v2（单链路 = laneID 0）；不适用的仅是 lane 建缩机制（A/§2.6）——HT v2 无 stripe 概念，POST/GET 池即其多路
- FrameLaneAck/FrameGauge 作为普通帧进 POST body / GET 响应；tx 通告经控制 POST（P1）发、回报经控制 GET（P3）回（wantTx 声明亦经 gossip，HT v2 同样周期 gossip）
- HT v2 的 Send/SendBatch 需补 tx 计量钩子（onSent）；rx 在 body 解析处累计
- HT v2 单链路视为 laneID 0，用 §3 的计量和门控覆盖
- ack 必须走隔离控制路径（P1–P3），否则 ack 延迟会节流链路
- 损失记账：发送方需计入已知损失（失败 POST 字节、服务端 CoDel 丢弃），否则 `tx − ackedRx` 泄漏导致门控楔住
- 规则：ack 持续到达但 `ackedRx` 不动超过 max(4×baseRTT, 2s) → 重置 baseline
- 复用 FrameLaneAck 空 payload 请求协商（B1 已实现，见 §3.2），不发 gossip；旧 peer 不收到未知帧
- 一旦门控生效，服务端批次自然 ≤ cap，512KB 队头问题解决；8MB cap 和 CoDel 成为后备

#### P8：CoDel 对称化
- 门控生效后，AQM 移到发送端 DRR 队列（两端）
- 若保留服务端 CoDel，需在帧进入 meshOut 时打时间戳，逐帧用真实时间评估

### 4.3 兼容性
- 保持 body format 不变（3B header + payload，整体 XChaCha20）
- 新行为通过请求头键控（旧服务端忽略）或 gossip flag
- 逐 chunk 密封需要 HEAD 请求中协商版本

### 4.4 与原设计的差距
原设计 `htunnel_p2p_direct_mode.md:147-151` 指定了独立控制通道，但未实现。本方案补齐该缺口，并解决 GET 侧队头阻塞、超时 teardown、DNS 优先级和门控损失记账问题。

## 5. 实施顺序
1. B 的计量 + 回报帧 + 能力协商（两端都要新版本才生效）
2. B 的门控
3. A 专用数据 lane
4. C HT v2
每步单测 + 本地编译通过后统一部署 VM/GG 验证，再部署其他节点。

## 6. 验收
- Mac→vm.phn 4 路并发下载 3 分钟：
  - VM/GG 不再出现 `link dead held` / `link dead`
  - 压测期间 `vm.phn` DNS 解析 <1s；对 vm.phn 的交互请求（`/api/version`）<3s
  - 总吞吐不低于现状（≈27KB/s）
- GG↔JF（直连 TCP，Linux）64MB 吞吐不低于现状
- QG→JF（HT v2）压测期间无 `link dead`，ping SRTT 不随数据量线性涨
- 新老版本混跑（只升一端）链路正常、无报错

## 7. 新增诊断日志
- `[LANE-INFLIGHT]` 每 5s（有门控时）：lane、inflight、cap、rate、baseRTT、门控累计等待时长
- `[STRIPE] dedicated data lane` 拨号/移除/重拨

## 8. 实施交接说明（B1 已完成，待 B2/A/C）

### 8.1 当前进度

**✅ B2 在途门控已实施**（2026-11-07，本地编译+单测通过，未提交未部署）：
- `p2p/lane_flow.go`: laneFlow 增加 capVal/lastAdjustAt/negQDRounds/ackedAdvancedAt/stallWarnedAt；`adjustCap`（LEDBAT 公式，qd 钳 0 不加速、持续 5s 负 qd 重建 minRTT 基线）、`gateState`、`stallCheck`（ackedRx 冻结 10s 且 inflight>0 → 解除门控 + `[LANE-STALLED]`）；`flowLoop` 每 tick 跑 adjustCap+stallCheck；`writeCredit`/`notifyCredit`/`onPeerLaneGossip`/`laneGossipFields`/`announceGaugeLocked`；B1 空 payload 语义废除（handleLaneAck 空则忽略），requestLaneAcks 已删
- `p2p/stripe_transport.go`: cfg 加 `onCredit`；结构体加 peerGauge(-1=无)/peerWantsTx/peerWantsGauge/gaugeSent；baseReadLoop 处理 0x18 FrameLaneGauge；Send/SendBatch 去掉 requestLaneAcks 调用；sampleSojourn/controllerTick 的 gauge 更新接 announceGaugeLocked（0.7 水位穿越即发）；controllerTick 门控生效（primaryFlow gated）冻结 addDue（§2.6 gauge 信号源切换）
- `p2p/p2p.go`: Peer 加 flowNotify（buf 1）；peerWriteLoop credit≤0 时不选 drrOutput（control/DNS 照走），credit 由 stripe.writeCredit() 供给、MaxInt64=不门控（stripe nil 或 peer 未 ack）；collectWriteBatch 加 maxDataBytes 按信用限数据批；两处 stripe 创建点接 onCredit→flowNotify；peerSender.LaneAckFields()；handleGossip 消费 Lack/WantTx/WantGauge/LaneRx/Gauge → onPeerLaneGossip
- `frame/frame.go`: FrameLaneGauge=0x18，0x17 注释更新（非空紧急通告）
- `mesh/topology.go`: GossipInfo 加 Lack/WantTx/WantGauge/LaneRx/GossipLaneRx/Gauge（per-link 注入，Go JSON 忽略未知字段 → 不升 P2PProtocolVersion）
- `mesh/mesh.go`: PeerSender 接口加 LaneAckFields()；gossip 构建注入（仿 GossipFields 契约）
- 测试 fake 补 LaneAckFields（3 个）；ipip_classify_test 的 collectWriteBatch 调用补 maxDataBytes 参数
- 验证：`go build` + `go test -vet=off ./mesh/ ./p2p/ ./frame/` 全过（mesh 包 vet 报的 2 处 netstack.go %w 为预存问题）

**✅ B1 LaneAck 计量框架** (commit 61a80ae) 已完成并推送：
- `frame/frame.go`: FrameLaneAck = 0x17, KernelBacklogLimited interface
- `frame/notsent_lowat.go`: limitKernelBacklog 返回 bool
- `frame/transport.go`: streamTransport.lowat + KernelBacklogLimited() 方法
- `p2p/lane_flow.go`: laneFlow（tx/rx/onSent/onRecv/onAck），minRTT 桶，rate EMA，encode/decode
- `p2p/stripe_transport.go`: primaryFlow/lane flow, handleLaneAck, flowLoop, requestLaneAcks, sendControl/name 字段
- `p2p/p2p.go`: passive/active side sendControl/name 传入，FrameLaneAck 静默忽略
- 编译验证: `go vet ./p2p/` 通过

### 8.2 待实现任务（按优先级顺序）

**🔴 B2: peerWriteLoop 在途门控 (§3.4)** — ✅ 已实施（见 §8.1，待部署验证）

目标：在 `peerWriteLoop` 出队处拦住超额数据，让多余数据留在 DRR 队列（公平调度、8MB 预算、满了丢包 → 内层 TCP 正常降速），控制帧永不被卡住。

关键步骤：
1. 在 `laneFlow` 添加 `cap int64` 字段和 `adjustCap(now)` 方法（LEDBAT-like 公式见 §3.3：**qd<0 按 0 处理且不加速**；minRTT 桶污染检测与基线重建必须实现）
2. 在 `flowLoop` 中每 200ms 调用 `adjustCap`
3. **gauge 信号源切换**：门控生效后原 sojourn gauge 会永远偏高（§2.6），controller 的加 lane 判据改为门控等待时长占比，或对门控已生效的 lane 冻结加 lane 决策
4. 修改 `peerWriteLoop`：计算 `credit = sum(caps) - sum(inflights)`，credit ≤ 0 时不选 drrOutput，但仍服务 controlCh/dnsQueue；ack 到达经 notify channel 唤醒（select 改造，现有 select 对 ready 通道随机选择）
5. 停滞防护：10s 无 ack 进展且 inflight > 0 → 告警 `[LANE-STALLED]`（gossip 兜底生效后 ackedRx 终会推进，此处退化为快速告警，不再承担防楔死职责）
6. **信令终审落地**：废除 B1 空 payload 语义（0x17 非空紧急通告 + 新增 0x18 紧急 gauge 通告）；gossip 新增 lack:true / wantTx / wantGauge 声明位 + tx/gauge 兜底字段（详见 §3.2）；ackRequested 改由对端 gossip 声明 wantTx 置位

关键文件：`p2p/p2p.go:peerWriteLoop` (line 625, 2026-11-07 对账修正——原交接写 ~1650 有误), `collectWriteBatch` (line 774), `p2p/lane_flow.go`

验收：压测 VM→GG 时 `[LANE-INFLIGHT]` 显示 inflight 稳定在 64–256KB，不再出现 `link dead held`

---

**🔴 A: 专用数据 lane (§2)** — 必须在 B2 完成后才能实现

目标：当主连接无法设上 TCP_NOTSENT_LOWAT 时（代理/中继/Windows），主动方拨一条专用数据 lane，所有可拆分数据帧只走该 lane，主连接只走控制/DNS/ping。

关键步骤：
1. dial 后检查 `frame.KernelBacklogLimited`，未实现或返回 false → 触发专用 lane
2. 3s 延迟后拨号，固定 stripeID = 0xFFFFFFFF，失败退避 5s→60s
3. `pickLanePinned` 排除 primary（候选集只剩专用 lane + 动态 lanes）
4. 专用 lane 豁免降级剔除和 5 分钟空闲回收
5. `laneEnqueue` 满时不回落主连接（由 B2 门控保证不会塞满）
6. **双向 lane 控制（§2.6）与 A 合并实施**：gossip 遥测字段（gauge/gated-wait，遵守 scope 约束）+ wantGauge 声明、主动端 controller 判据接入对端遥测（或扩、且缩）、gauge 越阈紧急通告走 FrameGauge（0x18）

关键文件：`p2p/p2p.go` dial path (~line 1480), `p2p/stripe_transport.go` attachLane/removeLane/controllerTick/pickLane

验收：VM→GG 链路建立后 ~3s 看到 `[STRIPE] dedicated data lane dialed`，压测期间主连接 Recv-Q ≈ 0

---

**🟡 C-P1/P2: HT v2 控制帧分离 (§4)** — 可独立于 B2/A 实施

**P1: 客户端控制 POST**
- isControl 帧单独小 POST，专用信号量 1–2 槽（不占 sendSem 16 槽），超时 5s
- 关键文件：`dialer/htunnel_direct.go`

**P2: 服务端入站分流**
- meshHandleWrite 中非 FrameMeshPacket 和 DNS 进 meshCtrlIn，Recv 先服务 meshCtrlIn
- handler 阻塞时限 2s，超时丢弃并告警
- 关键文件：`server/htunnel_mesh.go`

验收：QG→JF 压测期间 ping SRTT 不随数据量涨，服务端日志无 handler 阻塞 >2s

---

### 8.3 实施顺序与依赖

```
B2 (gate) → A (dedicated lane) → C-P1/P2 (HT v2 control separation)
```

原因：A 的专用 lane 队列（256 批次）需要 B2 的门控来防止进程内积压。C 可以独立实施，但建议等 B2 稳定后再做。

### 8.4 已知风险

1. **B2 cap 公式可能过于保守**：快链路可能被限流，需通过压测调整参数（target=200ms, ×1.25 加速步长）
2. **minRTT 桶污染**（2026-11-07 新增）：门控生效前后 RTT 数量级差几十倍，基线重建逻辑有漏洞会致 cap 涨回 8MB（§3.3 负 qd 防护）
3. **A 专用 lane 重拨风暴**：连续失败后退避到 60s 仍可能影响体验，考虑增加"放弃专用 lane"逻辑
4. **双向 lane 控制协调**（2026-11-07 新增，§2.6）：gossip 遥测的 scope 约束（全网 flood 的每节点字段 vs per-session gauge，只带最拥塞摘要或有界小表）；gossip 周期 vs 30s 判据窗口的匹配确认；对端遥测高但本地低时的加 lane 决策抖动（沿用 30s 持续约束）；遥测丢失（gossip 中断）时 controller 退化为仅本地判据
5. **HT v2 兼容性**：旧服务端不认识 `X-P2P-Control` 头，控制帧仍会排队，需通过 gossip 协商能力
6. **laneFlow.cap 并发访问**：adjustCap（写）和 peerWriteLoop（读）之间需要原子访问或加锁，考虑用 atomic.Int64
7. **停滞防护误触发**：慢链路（46KB/s）10s 无 ack 可能是正常的，需区分"慢但健康"和"真正停滞"——已按 ackedRx 冻结口径对齐（§3.3），实施时以"ack 到达但 ackedRx 不动"为判据

### 8.5 参考文档

- 问题跟踪: `docs/issues/vm-gg-ssh-proxy-link.md`
- Stripe 设计: `docs/plans/mesh-multilink-stripe-design.md`
- HT v2 设计: `docs/plans/htunnel_p2p_direct_mode.md`

### 8.6 2026-11-07 评审结论记录（代码对账 + 方案修订）

**对账修正（已并入正文）**：
1. peerWriteLoop 实际在 p2p.go:625（原交接 ~1650 有误）
2. gossip `lack:true` 从未进代码：它是本方案早期草稿的能力通告设计（仿已有 udp:true gossip 协商字段，见 b669dd6），c773979 曾用 FrameLaneAck 空 payload 懒协商取代。**2026-11-07 终审按"目的/频率分离"原则定稿：能力声明回归 gossip（lack:true 字段），空 payload 帧保留为会话级即时触发，两者分工见 §3.2**
3. §3.3 VM→GG 数值示例算错：55ms 是 minRTT 非 qd；并新增负 qd 防护与 minRTT 污染基线重建（B2 必做）
4. §4.1 "无隔离通道"过时：egress/客户端接收侧隔离已存在，真实缺口=客户端 POST 发送侧 + 服务端入站 Recv 侧

**机制结论**：
- **信令总原则（2026-11-07 终审）**：所有信息（能力、需求声明、tx 计量、gauge 遥测）都走 gossip 周期兜底通告，携带与否取决于对方声明的"要不要"；紧急变更用双专职帧立即通告——**FrameLaneAck（0x17）通告 tx（增量达阈值即发）、FrameGauge（0x18）通告 gauge（越阈即发），都是紧急通告不是协商帧**；无协商/触发帧，需求声明即协商（详见 §3.2）
- 双专职紧急通告帧：FrameLaneAck（0x17）= tx 计量通告（rx 增量 ≥50ms 有变化即发）；FrameGauge（0x18）= gauge 通告（上/下穿 0.7 水位即发）；两者内容 gossip 均周期兜底，携带取决于对方 wantTx/wantGauge 声明。两端各有一条独立计量回路
- **无风暴风险**：满载最坏 ~20 帧/s ≈ 2KB/s（46KB/s 瓶颈链路上 ~4%），且只在有数据变化时发；ack 走主连接控制优先级、不被门控卡，回路闭合
- 动态 lane 原实现仅主动端有控制器（p2p.go:1482 vs 1127，controllerLoop 仅 dialLane != nil 时启动）；**已修订为双向覆盖、主动端单方决策（§2.6）**：两端状态都以遥测上报（LaneAck 捎带 1B gauge/gated-wait，仅发已 gated 对端即天然协商），扩=本地或对端超阈，缩=双低，无 lane-request 帧——被动端保持纯发送方，与无 controllerLoop 的现状一致
- LaneAck 计量/门控天然按方向独立：被动端虽无扩容杠杆（修订前），但 B2 门控对其发送方向照常生效

**实施顺序维持** B2 → A（含 §2.6 双向控制）→ C-P1/P2 不变。

**最后更新**: 2026-11-07（评审修订） | **下一个接手者**: 请阅读本节 + §2.6/§3.3/§8.2，按优先级顺序实施
