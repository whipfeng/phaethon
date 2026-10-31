# 链路质量与选路改进 v2

## 背景

排查 GG 方向"时通时不通"暴露的三层问题:

1. **测量盲区**:cost 基于 ping/pong 实测 SRTT,而 ping/pong 走 `controlCh` 优先队列(p2p.go:643),对数据面拥塞和中转丢包完全无感。事故期间全网拓扑显示 lossRate=0、SRTT 正常,数据面实际在丢——路由系统对正在发生的故障零感知。
2. **状态冻僵**:`PeerQuality.calculateConfidence()`(quality.go:49)写好但无调用方,久不更新的链路永远用最后一次的好数据,路由不 adaptation。
3. **选路不稳定**:未探测哨兵 cost=1000 在隧道重连(seq 变化)时引发选路翻盘;无跳数惩罚使多跳中转的隐性风险(故障域、转发随机性)被低估;10s 重算无迟滞,噪声内两条路径来回翻。

## 设计总览

| 阶段 | 内容 | 线上行为风险 |
|------|------|------|
| A 本地计算 | 先验默认 / 置信度衰退 / 跳数惩罚 / 切换迟滞 | 零(纯数学) |
| B 出口健康入 gossip | drop 速率 + sojourn 时延作为链路质量字段广播 | 零(只加字段) |
| C 探测数据面化 | ping 走数据队列 + 迟到 pong 翻案 | 低(拥塞链路 SRTT 上升,行为变化符合预期) |
| 三期 | 端到端 IP 层探针(换路触发器) | 后续单独立项 |

前作:`link_quality_improvements.md`(ACK 被动测量 v7,已落地)。

## Phase A:本地计算

### A1 未探测链路 = 首个采样点

`NewPeerQuality()`(mesh/quality.go:23)初始化为虚拟采样:

```go
ackRTT: 100 * time.Millisecond, ackLossRate: 0, ackJitter: 0,
ackSampleCount: 1, ackUpdated: time.Now(),
```

- 删除 mesh.go:3110-3116 / 3154-3157 两处 `SRTT==0 → 1000` 哨兵特判
- cost 统一为一条公式(配合 A2):`Cost = SRTT / max(1-LossRate, 0.01)`
- 100ms 的取值:比典型直连(10-50ms)差(已探测链路自然淘汰),远好于 1000(多跳路径不再被压死);seq 重建时 cost 25→100,选路不翻盘
- 全网一起升级,不做旧版本兼容

### A2 置信度衰退

`buildTopologyGraph` 改用 `StatsWithConfidence()`(代码现成):

```
confidence = min(1, sampleCount/10) · exp(-age分钟数)
Cost = SRTT / max(1-LossRate, 0.01) / confidence
```

- 久不更新的链路 confidence → 0,cost → 巨大,自然被绕开;数据回来即恢复
- 与 A1 自洽:新链路 sampleCount=1 → confidence=0.1 → cost 放大 10 倍,语义等于"没把握的值先当 1000 用"
- 死链路持有的过期好数据不再永久占据路由表

### A3 跳数惩罚

Dijkstra 松弛时(mesh.go:3242):

```go
newCost := nodes[minNode].cost + bestCost + hopPenalty  // hopPenalty = 5ms
```

- Babel 标准做法:加法模型保留延迟可加性,每跳加常量补偿多跳的隐性代价(多一个故障域、多一次排队转发、多一分黑洞概率)
- 量级取小(5ms):是"助推"不是"改写"——2 跳 10+10 仍胜 1 跳 25;只在近似平局时倾向少一跳
- GG 单桥拓扑无备选路径时无效果,但拓扑丰富的节点(qg、jf)在等代价时自然减少中转依赖

### A4 切换迟滞

路径重算后(mesh.go:3256 更新逻辑外),对比新旧 bestPath:

```go
采用新 nextHop 当且仅当:newCost < currentCost × (1 - hysteresis)  // hysteresis = 0.15
```

- 15% 以内不换,只换实质性改善。两条路径噪声内(82.8 vs 85)不再每 10s 翻一次
- 这是治"时通时不通"现象最直接的一刀:抖动往往不是因为链路真变了,而是 cost 在阈值附近振荡

## Phase B:出口健康入 gossip

**动机**:中转黑洞发生在哪个节点,哪个节点自己最清楚(GG 事故是 qg 中转转发丢包,但 qg 以外所有节点都看不见)。本机 egress 统计接入 cost 后,qg 自己观测到自己转发在丢 → 抬高出向链路 cost → gossip 全网 → 集体绕开。

**测量**(p2p 层,每 peer):
- **drop 速率**:`enqueueWriteReq` 的 RED 丢弃计数(dataQueueDrops/dnsQueueDrops/ackQueueDrops)已有,qualityLoop 每 10s 采样增量 / 窗口发送量 = dropRate
- **sojourn 时延**:`writeReq` 加 `enqueueTime`,`peerWriteLoop` 真正 write 时算 `now - enqueueTime`,EMA 平滑(per peer)

**广播**:`GossipLink` 增加 `DropRate`、`SojournMs` 字段;直连链路用本机实测,gossip 链路用对端报来的值。

**Cost 公式**(A2 基础上):

```
lossEff  = 1 - (1 - lossPing) · (1 - dropRate)
Cost     = (SRTT + SojournMs) / max(1 - lossEff, 0.01) / confidence + hopPenalty
```

- 本地 drop 的包确实没到达对端,并入 loss 语义正确;sojourn 与 SRTT 同量纲(ms)直接相加

## Phase C:探测数据面化

**动机**:priority-queue ping 测不到拥塞。让主动探测和数据同遭遇,SRTT 才能反映去程排队延迟。

- **ping 入数据队列**:`sendPing` 改 `isControl=false`。`enqueueWriteReq` 修正:`isDNSPacket`/`isACKPacket` 只对 `FrameMeshPacket` 生效(只有 mesh 数据帧是裸 IP 包;JSON 控制载荷被当 IP 头解析是潜在 bug,顺带修掉)。ping 落 DRR 数据队列,与数据公平竞争
- **pong 保持优先队列**(controlCh 不变):回程零排队,SRTT ≈ 基础 RTT + 去程排队延迟
- **enqueue 拒绝(RED)仍回滚**:`cancelPing` 语义保留——未上 wire 不计丢包
- **迟到 pong 翻案**:pingTimeout(5s)判丢的条目进入 `lateGrace`(30s)宽限;迟到的 pong 到达时:丢包窗口中对应样本改 acked、`consecutiveLost=0`、完整 RTT 入采样(拥塞信号留在 SRTT,lossRate 只反映真丢包)
  - `samples` 窗口从 `[]bool` 改为带 seq 的结构以支持定位翻案
- **断链启发式维持现状**(连续 10 判丢 close):语义变为"极端持续拥塞或真死",两者都该重连;配合 A4 迟滞与 B 的 cost 信号,拥塞链路在被杀前流量已被引走,不会出现重连加剧拥塞的死循环

### u-turn 回抛探测器(环路实锤)

**动机**:各节点独立 Dijkstra + gossip 视图不一致可形成瞬态转发环路(外层 TTL 每跳递减,环路放大 ≤64 倍后自灭,期间路径=黑洞,10s 重算可能解环也可能重建——表现为"时通时不通")。需要廉价手段实锤/排除环路嫌疑。

**做法**:`HandleMeshFrame` 收到**任意中转帧**时(VIP/hostIP/GIP/EIP/fakeIP 全落在目标节点 /16 subnet 内,dst mask 后反查 owner),若 `bestPaths[target].NextHop` **== 帧来源节点** 即为 u-turn 回抛:Warn(限频) + 计数器,计数随 admin 统计出口暴露。覆盖两条转发路径:mesh CIDR 裸包中转 + IPIP 封装中转(EIP 也在 subnet 内,统一处理)。

**顺带修复**:`enqueueWriteReq` 的 `isDNSPacket`/`isACKPacket` 分类器只对 `FrameMeshPacket` 生效——JSON 控制帧载荷被当 IP 头解析是潜在误分类 bug。

## 三期:端到端 IP 层探针(不在本轮)

- 从 netstack 发 ICMP echo 到各节点 VIP,reply 天然走数据面,统计端到端丢包/延迟
- **定位是"换路触发器"不是"诊断器"**:能发现"这条端到端路径差"但归因不到具体跳;仅在端到端丢包显著大于各跳合成时惩罚当前 best path
- 稀疏拓扑(单桥场景)无备选路径时价值有限,待拓扑丰富后单独立项

## 影响面

| 文件 | 改动 |
|------|------|
| mesh/quality.go | A1 默认初始化 |
| mesh/mesh.go | A2 换 StatsWithConfidence、A3 hopPenalty、A4 迟滞、B cost 公式 |
| p2p/p2p.go | B drop 窗口采样 + sojourn EMA、GossipLink 字段;C ping 入数据队列 + pingTracker 翻案改造 |
| gossip 结构 | B 加 DropRate/SojournMs |

## 验证

1. 单元测试:置信度衰减曲线、迟滞边界(14.9% 不换/15.1% 换)、翻案(迟到/超宽限/无对应)、drop 速率窗口、sojourn EMA、默认初始化透出
2. 本地双节点:打流时 SRTT 上升、cost 上升、路由切走;停流后 confidence 衰减、cost 回落
3. 部署走标准门控:VM first → 用户确认 → QG → 其余

## 关键参数

| 参数 | 值 | 说明 |
|------|-----|------|
| 未探测先验 RTT | 100ms | 硬编码,虚拟首个采样点 |
| 跳数惩罚 | 5ms/跳 | 硬编码 |
| 切换迟滞 | 15% | 新路径需优于现路径 15% 才换 |
| 迟到宽限 | 30s | pingTimeout 的 6 倍 |
| confidence 时间常数 | 1 分钟 | 复用现有 calculateConfidence |
