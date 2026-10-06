# Link Quality Measurement Improvements

## 背景

当前 mesh 网络的链路质量测量（延迟、丢包率）存在多个问题，影响路由选择的准确性。本文档提出改进方案，提升质量测量的准确性和鲁棒性。

## 设计原则

### 采样源：控制帧 + 主动探测（不改协议）

**关键决策**：只从控制帧（ACK）和主动探测（heartbeat）采样，不从数据帧采样。

**理由**：
1. **ACK 已提供所有需要的信息**：
   - RTT：从发送数据帧到收到 ACK 的时间（往返延迟）
   - 丢包率：ACK 收到 = 没丢，ACK 超时 = 丢了
   - 采样率 = 数据流量速率（每个数据帧产生一个 ACK）

2. **数据帧不能提供额外价值**：
   - 单向延迟需要时钟同步（不现实）
   - 吞吐量/带宽不是路由决策指标
   - 包大小影响过于细节

3. **主动探测补充空闲时段**：
   - heartbeat/ping 提供空闲时的基础质量数据
   - 与控制帧采样互补，覆盖所有场景

**结论**：控制帧 + 主动探测已经足够，不需要从数据帧额外采样。

## 当前算法

### RTT 计算
- **方法**：TCP 风格平滑 RTT（RFC 6298）
- **公式**：`SRTT = (7/8) * SRTT + (1/8) * R`（alpha = 1/8）
- **样本窗口**：最近 100 个 ACK（**样本计数窗口**）
- **来源**：被动测量（从发送到收到 ACK 的时间）

### 丢包率计算
- **方法**：时间窗口统计
- **窗口**：最近 60 秒的 frameHistory（**时间窗口**）
- **公式**：`lossRate = lost / sent`
- **"lost" 定义**：达到最大重试次数后放弃的帧

### 路径代价
- **公式**：`effectiveRTT = SRTT / (1 - lossRate)`
- **用途**：路由选择（代价越低越好）

## 问题分析

### 问题 1：RTT 和丢包率的窗口机制不一致
- **RTT**：基于样本计数（100 个样本）
- **丢包率**：基于时间窗口（60 秒）
- **问题**：
  - 语义不一致：RTT 的"最近"取决于流量速率
  - 高流量时：100 个样本可能只有 1 秒（过于敏感）
  - 低流量时：100 个样本可能需要 1 小时（过于迟钝）
  - 难以推理数据的"新鲜度"

### 问题 2：丢包率统计偏差
- **只统计"彻底丢失"**（达到 max retries 后放弃的帧）
- **不统计"仍在飞行中"的帧**（pending 队列里的）
- **后果**：如果网络延迟突然增大，大量帧卡在 pending 里，lossRate 会**低估**真实丢包率

### 问题 3：RTT 采样偏差
- **只采样成功 ACK 的 RTT**
- **不区分"首次传输"和"重传"**
- **后果**：重传的 RTT 会拉高 SRTT，但实际上应该区分：
  - 原始传输 RTT（真实网络延迟）
  - 重传 RTT（包含重传延迟，偏高）

### 问题 4：缺少抖动（Jitter）指标
- **现状**：只有 SRTT 和 lossRate
- **缺失**：RTT 方差（虽然 rttSampler 内部有 rttvar，但没暴露）
- **影响**：对于实时应用（语音、视频），抖动比平均延迟更重要

### 问题 5：effectiveRTT 公式的局限
- **公式**：`effectiveRTT = SRTT / (1 - lossRate)`
- **数学假设**：丢包是独立事件（几何级数模型）
- **实际问题**：网络丢包往往是**突发性的**（burst loss），不是独立的
- **极端情况**：lossRate → 1.0 时，代价 → ∞，可能导致路径震荡

### 问题 6：没有时间衰减
- **现状**：qualityTracker 里的数据没有过期机制
- **问题**：如果某个 peer 长时间没有流量，stats 会一直保留旧值
- **后果**：路由选择可能基于**过时的数据**

## 设计方案

### 改进 1：统一时间窗口机制（5 分钟长记忆）

**目标**：RTT 和丢包率都使用一致的时间窗口，延长到 5 分钟以保留更多历史数据

**方案**：
- **窗口大小**：5 分钟（300 秒，而非 60 秒）
- **RTT 统计**：保留最近 5 分钟内的所有 RTT 样本
- **SRTT 计算**：基于时间窗口内的样本计算（而非全局平滑）
- **理由**：控制帧采样率依赖数据流量，长记忆确保低流量时也有足够数据

**实现**：
```go
type rttSampler struct {
    mu      sync.Mutex
    samples []rttRecord  // 带时间戳的样本
    window  time.Duration // 5 分钟
}

type rttRecord struct {
    timestamp time.Time
    rtt       time.Duration
    isRetransmission bool // 标记是否为重传
}

func (s *rttSampler) add(rtt time.Duration, isRetransmission bool) {
    s.mu.Lock()
    defer s.mu.Unlock()
    
    // 添加新样本
    s.samples = append(s.samples, rttRecord{
        timestamp: time.Now(),
        rtt: rtt,
        isRetransmission: isRetransmission,
    })
    
    // 清理过期样本
    s.cleanup()
}

func (s *rttSampler) cleanup() {
    cutoff := time.Now().Add(-s.window)
    var recent []rttRecord
    for _, r := range s.samples {
        if r.timestamp.After(cutoff) {
            recent = append(recent, r)
        }
    }
    s.samples = recent
}

func (s *rttSampler) getSRTT() time.Duration {
    s.mu.Lock()
    defer s.mu.Unlock()
    
    s.cleanup()
    
    // 只用首次传输的样本计算 SRTT
    var sum time.Duration
    var count int
    for _, r := range s.samples {
        if !r.isRetransmission {
            sum += r.rtt
            count++
        }
    }
    
    if count == 0 {
        return 0
    }
    return sum / time.Duration(count)
}

func (s *rttSampler) getSampleCount() int {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.cleanup()
    
    count := 0
    for _, r := range s.samples {
        if !r.isRetransmission {
            count++
        }
    }
    return count
}
```

**优点**：
- ✅ 语义一致：RTT 和丢包率都基于"最近 5 分钟"
- ✅ 长记忆：低流量链路也能保留历史数据
- ✅ 自适应：高流量和低流量下行为一致
- ✅ 易于推理：数据的"新鲜度"明确

**缺点**：
- ⚠️ 低流量时样本少，统计不准确
- ⚠️ 需要管理时间-based 清理

**缓解措施**：
- ✅ 添加置信度指标（见改进 6）
- ✅ 样本不足时降低路由优先级

### 改进 2：区分首次传输和重传的 RTT

**目标**：只用首次传输的 RTT 计算 SRTT

**方案**：
- 在 `rttRecord` 中添加 `isRetransmission` 字段
- `getSRTT()` 只使用 `isRetransmission == false` 的样本
- 重传的 RTT 仍然记录，但不参与 SRTT 计算

**实现**：
```go
// 在 sendState.onACK() 中
if pf, ok := s.pending[seq]; ok {
    rtt := time.Since(pf.sentTime)
    isRetrans := pf.retries > 0
    s.rttSampler.add(rtt, isRetrans)
    // ...
}
```

**优点**：
- SRTT 更准确反映真实网络延迟
- 重传延迟不会污染 SRTT

### 改进 3：丢包率考虑 pending 队列 + 5 分钟窗口

**目标**：更准确地估计丢包率，统一使用 5 分钟窗口

**方案**：
- **窗口**：5 分钟（300 秒，与 RTT 一致）
- **考虑 pending**：统计"可能丢失"的超时帧
- **公式**：`lossRate = (lost + pendingOld) / (sent + pendingOld)`

**实现**：
```go
func (s *sendState) getStats() (srtt, rto, jitter time.Duration, lossRate float64, sampleCount int) {
    s.mu.Lock()
    defer s.mu.Unlock()
    
    s.cleanupOldRecords() // 清理 5 分钟前的记录
    
    srtt = s.rttSampler.getSRTT()
    rto = s.rttSampler.getRTO()
    jitter = s.rttSampler.getJitter()
    sampleCount = s.rttSampler.getSampleCount()
    
    // 计算丢包率（5 分钟窗口）
    var sent, lost, pendingOld int
    now := time.Now()
    
    // 统计已完成的帧（5 分钟内）
    for _, r := range s.frameHistory {
        sent++
        if r.lost {
            lost++
        }
    }
    
    // 统计"可能丢失"的 pending 帧
    // 如果 pending 时间超过 2 * RTO，认为可能丢失
    for _, pf := range s.pending {
        if now.Sub(pf.sentTime) > 2 * s.rttSampler.getRTO() {
            pendingOld++
        }
    }
    
    // 丢包率 = (彻底丢失 + 可能丢失) / (已完成 + 可能丢失)
    totalCompleted := sent
    totalWithPending := sent + pendingOld
    
    if totalWithPending > 0 {
        lossRate = float64(lost + pendingOld) / float64(totalWithPending)
    }
    
    // Log stats when queried
    util.LogInfo("[P2P-STATS] window=%ds sent=%d lost=%d pendingOld=%d lossRate=%.4f samples=%d", 
        300, sent, lost, pendingOld, lossRate, sampleCount)
    return
}
```

### 改进 4：暴露抖动指标

**目标**：提供 RTT 方差（抖动）指标

**方案**：
- 在 `getStats()` 中返回 rttvar
- 在路径代价计算中使用抖动

**实现**：
```go
func (s *sendState) getStats() (srtt, rto, jitter time.Duration, lossRate float64) {
    s.mu.Lock()
    defer s.mu.Unlock()
    
    s.cleanupOldRecords()
    
    srtt = s.rttSampler.getSRTT()
    rto = s.rttSampler.getRTO()
    jitter = s.rttSampler.getJitter() // 新增
    lossRate = s.calculateLossRate()
    
    return
}

func (s *rttSampler) getJitter() time.Duration {
    s.mu.Lock()
    defer s.mu.Unlock()
    
    s.cleanup()
    
    // 计算样本的标准差
    if len(s.samples) < 2 {
        return 0
    }
    
    var sum time.Duration
    for _, r := range s.samples {
        if !r.isRetransmission {
            sum += r.rtt
        }
    }
    mean := sum / time.Duration(len(s.samples))
    
    var variance time.Duration
    for _, r := range s.samples {
        if !r.isRetransmission {
            diff := r.rtt - mean
            variance += diff * diff
        }
    }
    variance /= time.Duration(len(s.samples))
    
    // 返回标准差
    return time.Duration(math.Sqrt(float64(variance)))
}
```

### 改进 5：改进路径代价公式 + 传播 Jitter

**目标**：更鲁棒的路径代价计算，传播 Jitter 用于多跳路径选择

**方案**：
- **公式**：`cost = SRTT * (1 + lossRate) + jitter`
- **多跳 Jitter**：`totalJitter = sqrt(sum(linkJitter²))`（假设各跳独立）
- **传播 Jitter**：在 `GossipLink` 中添加 `Jitter` 字段

**GossipLink 结构**：
```go
type GossipLink struct {
    LocalSeq     uint16
    RemoteSeq    uint16
    FriendlyName string
    SRTT         float64 // ms
    LossRate     float64
    Jitter       float64 // ms，新增
}
```

**多跳路径代价计算**：
```go
func calculatePathCost(path []GossipLink) float64 {
    var totalRTT float64
    var totalSuccess float64 = 1.0
    var totalJitterSquared float64
    var hasJitterData bool
    
    for _, link := range path {
        totalRTT += link.SRTT
        totalSuccess *= (1 - link.LossRate)
        
        // 只有当 Jitter > 0 时才使用（向后兼容旧节点）
        if link.Jitter > 0 {
            totalJitterSquared += link.Jitter * link.Jitter
            hasJitterData = true
        }
    }
    
    totalLoss := 1 - totalSuccess
    cost := totalRTT * (1 + totalLoss)
    
    // 如果有 jitter 数据，加入代价计算
    if hasJitterData {
        cost += math.Sqrt(totalJitterSquared)
    }
    
    return cost
}
```

**向后兼容**：
- ✅ 旧节点不广播 Jitter（字段为 0）
- ✅ 新节点检测到 `Jitter == 0` 时只用 SRTT + LossRate
- ✅ 混合网络中新旧节点可以共存

### 改进 6：时间衰减 + 置信度机制

**目标**：避免使用过时的质量数据，区分数据的可靠性

**方案**：
- **时间衰减**：在 `PeerQuality` 中添加 `LastUpdate` 时间戳
- **置信度**：基于样本数和时间新鲜度计算
- **路由选择**：低置信度的路径降低优先级

**实现**：
```go
type PeerQuality struct {
    mu sync.RWMutex
    
    ackRTT      time.Duration
    ackLossRate float64
    ackJitter   time.Duration
    sampleCount int        // 最近 5 分钟内的样本数
    ackUpdated  time.Time
}

// StatsWithConfidence returns quality metrics with confidence score
func (q *PeerQuality) StatsWithConfidence() (avgRTT time.Duration, loss, jitter float64, confidence float64) {
    q.mu.RLock()
    defer q.mu.RUnlock()
    
    avgRTT = q.ackRTT
    loss = q.ackLossRate
    jitter = q.ackJitter
    
    // 计算置信度
    confidence = q.calculateConfidence()
    
    return
}

func (q *PeerQuality) calculateConfidence() float64 {
    // 样本数因子：至少需要 10 个样本才有统计意义
    sampleFactor := math.Min(1.0, float64(q.sampleCount) / 10.0)
    
    // 时间因子：最近更新的数据更可信（1 分钟衰减）
    age := time.Since(q.ackUpdated)
    timeFactor := math.Exp(-float64(age.Minutes()))
    
    return sampleFactor * timeFactor
}

func (q *PeerQuality) IsStale(ttl time.Duration) bool {
    q.mu.RLock()
    defer q.mu.RUnlock()
    return time.Since(q.ackUpdated) > ttl
}
```

**路由选择时使用置信度**：
```go
avgRTT, loss, jitter, confidence := quality.StatsWithConfidence()

if confidence < 0.3 {
    // 低置信度：降低优先级或排除
    effectiveCost = math.MaxFloat64
} else {
    // 高置信度：正常使用
    effectiveCost = float64(avgRTT.Milliseconds()) * (1 + loss) + jitter.Milliseconds()
    effectiveCost /= confidence // 置信度越高，代价越低
}
```

## 实现计划

### 阶段 1：核心改进（必须）

**文件**：`p2p/reliable.go`

1. ✅ **修改 rttSampler**：
   - 改用时间窗口（5 分钟）而非样本计数（100 个）
   - 添加 `rttRecord` 结构（带时间戳和 `isRetransmission` 标记）
   - 实现 `cleanup()` 方法清理过期样本
   - 修改 `add()` 方法接受 `isRetransmission` 参数
   - 修改 `getSRTT()` 只用首次传输的样本
   - 添加 `getSampleCount()` 方法
   - 添加 `getJitter()` 方法（计算标准差）

2. ✅ **修改 sendState**：
   - 修改 `onACK()` 传递 `isRetransmission` 参数（`pf.retries > 0`）
   - 修改 `getStats()` 返回 `jitter` 和 `sampleCount`
   - 修改 `cleanupOldRecords()` 使用 5 分钟窗口
   - 修改丢包率计算考虑 pending 队列

**文件**：`p2p/p2p.go`

3. ✅ **修改 GetLinkQualityStatsByProxy**：
   - 返回值添加 `jitter` 和 `sampleCount`

**文件**：`mesh/quality.go`

4. ✅ **修改 PeerQuality**：
   - 添加 `ackJitter` 字段
   - 添加 `sampleCount` 字段
   - 修改 `UpdateACKStats()` 接受 jitter 和 sampleCount 参数
   - 修改 `Stats()` 返回 jitter
   - 添加 `StatsWithConfidence()` 方法
   - 添加 `calculateConfidence()` 方法

**文件**：`mesh/mesh.go`

5. ✅ **修改 updateACKStats**：
   - 从 P2P 层获取 jitter 和 sampleCount
   - 传递给 `quality.UpdateACKStats()`

6. ✅ **修改路径选择逻辑**：
   - 使用 `StatsWithConfidence()` 获取置信度
   - 低置信度路径降低优先级
   - 路径代价公式：`cost = SRTT * (1 + lossRate) + jitter`
   - 最终代价：`effectiveCost = cost / confidence`

### 阶段 2：测试验证

1. ✅ **单元测试**：
   - RTT 采样（区分首次/重传）
   - 丢包率计算（考虑 pending）
   - 抖动计算
   - 置信度计算
   - 时间窗口清理

2. ✅ **集成测试**：
   - 在 VM 环境部署
   - 监控质量指标的变化
   - 验证路径选择是否合理

3. ✅ **日志验证**：
   - 检查 `[P2P-STATS]` 日志
   - 确认 sampleCount、jitter、lossRate 合理
   - 确认置信度计算正确

## 关键参数

| 参数 | 值 | 说明 |
|------|-----|------|
| 时间窗口 | 5 分钟（300 秒） | RTT 和丢包率统一使用 |
| 最小样本数 | 10 | 低于此值置信度下降 |
| 置信度阈值 | 0.3 | 低于此值路径降低优先级 |
| 时间衰减常数 | 1 分钟 | 置信度的时间因子 |
| Pending 超时 | 2 * RTO | 超过此值认为可能丢失 |

## 向后兼容性

- ✅ 不需要改变节点间通信协议（帧格式、ACK 格式）
- ⚠️ 在 `GossipLink` 中添加 `Jitter` 字段（向后兼容）
  - 旧节点不广播 Jitter（字段为 0）
  - 新节点检测到 `Jitter == 0` 时只用 SRTT + LossRate
  - 混合网络中新旧节点可以共存
- ✅ 所有统计和计算逻辑都是本地改进

## 参考文献

- RFC 6298: Computing TCP's Retransmission Timer
- TCP BBR: Congestion-Based Congestion Control
- Babel Routing Protocol: Link quality metrics
