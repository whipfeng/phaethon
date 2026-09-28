# P2P 协议 v7 — 控制帧可靠传输 + 链路质量感知

> **文档类型**：Plan  
> **版本**：1.0.0  
> **创建日期**：2026-09-28  
> **最后更新**：2026-09-28

---

## 一、背景与动机

### 1.1 当前问题

1. **控制帧不可靠**：h_tunnel 传输层使用异步 POST，控制帧可能丢失或乱序
2. **链路质量依赖主动探测**：需要单独的 probe/probe_reply 帧测量 RTT 和丢包率
3. **帧类型冗余**：heartbeat、probe、probe_reply 功能可合并到 gossip

### 1.2 设计目标

1. **控制帧可靠送达**：seq/ack + 超时重传
2. **链路质量被动采样**：从 ack 计算 RTT 和丢包率，无需专门探测帧
3. **简化帧类型**：只保留 hello 和 gossip 两种控制帧

---

## 二、协议变更

### 2.1 帧格式

**控制帧**（hello、gossip）：
```
┌──────────┬──────────┬──────────┬──────────┬─────────────┐
│ type (1) │ seq (4)  │ ack (4)  │ len (2)  │ payload ... │
└──────────┴──────────┴──────────┴──────────┴─────────────┘
```

| 字段 | 大小 | 说明 |
|------|------|------|
| type | 1 byte | 帧类型（hello=0x01, gossip=0x02） |
| seq | 4 bytes | 本帧序号，单调递增 |
| ack | 4 bytes | 最近收到的对端 seq（0 表示无） |
| len | 2 bytes | payload 长度 |
| payload | 可变 | JSON 编码的控制数据 |

**数据帧**（mesh packet）：
```
┌──────────┬──────────┬─────────────┐
│ type (1) │ len (2)  │ payload ... │
└──────────┴──────────┴─────────────┘
```

数据帧不变，fire-and-forget，无 seq/ack。

### 2.2 帧类型

| 类型 | 值 | 说明 | 可靠传输 |
|------|-----|------|----------|
| hello | 0x01 | 建交握手 | ✅ |
| gossip | 0x02 | 拓扑更新 + 保活 + 链路质量采样 | ✅ |
| mesh_packet | 0x10 | 数据帧 | ❌ |

**删除的帧类型**：
- heartbeat（gossip 周期性发送，兼做保活）
- probe / probe_reply（ack 机制提供链路质量采样）

### 2.3 协议版本

```go
const P2PProtocolVersion = 7
```

版本变更说明：
- v7：控制帧加 seq/ack，删除 heartbeat/probe/probe_reply

---

## 三、可靠传输机制

### 3.1 发送端

每个 peer 维护：
```go
type peerSendState struct {
    nextSeq     uint32              // 下一个发送序号
    pending     map[uint32]*pendingFrame // 已发送未确认的帧
    lastAck     uint32              // 最近收到的 ack
}

type pendingFrame struct {
    frameType   byte
    payload     []byte
    sentTime    time.Time           // 最近发送时间
    retries     int                 // 重传次数
}
```

**发送流程**：
1. 分配 seq（nextSeq++）
2. 填入 ack（最近收到的对端 seq）
3. 记录到 pending map
4. 启动重传定时器

**重传策略**：
- 初始超时：1s
- 指数退避：1s → 2s → 4s → 8s
- 最大重传次数：5 次
- 超过后判定连接死亡，触发重连

### 3.2 接收端

每个 peer 维护：
```go
type peerRecvState struct {
    lastSeq     uint32              // 最近收到的 seq（用于生成 ack）
    seen        map[uint32]time.Time // 去重窗口
}
```

**接收流程**：
1. 检查 seq 是否重复（在 seen map 中）
2. 重复帧：丢弃，但仍发 ack
3. 新帧：更新 lastSeq，处理 payload，记录到 seen
4. 发送 ack（捎带在下一个发出的帧中）

**去重窗口**：
- 保留最近 1000 个 seq 的记录
- 或 60 秒内的记录

### 3.3 ACK 捎带

ack 不单独发送，而是捎带在每个发出的控制帧中：

```
发送 gossip 时：
  seq = nextSeq++
  ack = recvState.lastSeq  // 最近收到的对端 seq
```

如果长时间没有控制帧要发，发送空 gossip（只有 seq/ack，payload 为空）作为 keepalive。

---

## 四、链路质量测量

### 4.1 RTT 测量

从 pending map 中，当收到 ack 时：
```go
func (s *peerSendState) onAck(ack uint32, recvTime time.Time) {
    if pending, ok := s.pending[ack]; ok {
        rtt := recvTime.Sub(pending.sentTime)
        s.rttSampler.Add(rtt)
        delete(s.pending, ack)
    }
}
```

RTT 采样使用滑动窗口平均：
```go
type rttSampler struct {
    samples []time.Duration
    srtt    time.Duration  // smoothed RTT
    rttvar  time.Duration  // RTT variance
}
```

### 4.2 丢包率测量

从发送统计计算：
```go
type lossTracker struct {
    totalSent     uint64
    totalAcked    uint64
    lossRate      float64  // 滑动窗口丢包率
}
```

每收到一个 ack，更新统计。超时重传的帧算作丢包。

### 4.3 采样频率

gossip 周期 = 15s，每个 peer 每 15s 至少一次采样。

如果需要更高频率，可以：
- 缩短 gossip 周期
- 或在数据帧多时额外发空 gossip（仅用于 ack）

---

## 五、帧类型处理

### 5.1 Hello

**发送时机**：连接建立后立即发送

**处理逻辑**：
```go
func handleHello(peer *Peer, info GossipInfo) {
    // 1. 版本检查
    if info.ProtocolVersion != P2PProtocolVersion {
        // 版本不匹配，断开
    }
    
    // 2. 提取 nodeID，注册 peer
    nodeID := extractNodeID(info)
    peer.NodeID = nodeID
    peer.meshSender = &peerSender{peer: peer, nodeID: nodeID}
    m.meshHandler.RegisterPeer(peer.meshSender)
    
    // 3. 处理拓扑
    processGossipInfo(peer, info)
    
    // 4. 回复 hello
    sendHello(peer)
}
```

### 5.2 Gossip

**发送时机**：周期性（15s），兼做保活

**处理逻辑**：
```go
func handleGossip(peer *Peer, info GossipInfo) {
    // 首次收到 = 建交（peer.NodeID 为空）
    if peer.NodeID == "" {
        peer.NodeID = extractNodeID(info)
        peer.meshSender = &peerSender{peer: peer, nodeID: peer.NodeID}
        m.meshHandler.RegisterPeer(peer.meshSender)
    }
    
    // 更新拓扑
    processGossipInfo(peer, info)
}
```

### 5.3 Peer 注销

只在连接断开时注销：
```go
defer func() {
    if peer.meshSender != nil {
        m.meshHandler.UnregisterPeer(peer.meshSender)
    }
}()
```

---

## 六、传输层适配

### 6.1 FrameTransport 接口

接口不变：
```go
type FrameTransport interface {
    Send(frameType byte, payload []byte, isControl bool) error
    Recv() (frameType byte, payload []byte, err error)
    Close() error
}
```

但语义变化：
- 控制帧（isControl=true）：可靠送达，有序
- 数据帧（isControl=false）：尽力交付，可丢可乱序

### 6.2 h_tunnel 传输

控制帧改为同步 POST（保证送达）：
```go
func (t *htunnelDirectTransport) Send(frameType byte, payload []byte, isControl bool) error {
    if isControl {
        return t.sendControlSync(frameType, payload)  // 同步，等结果
    }
    return t.sendDataAsync(frameType, payload)  // 异步，fire-and-forget
}
```

### 6.3 streamTransport

TCP 天然可靠有序，无需额外处理。

---

## 七、删除的代码

| 文件 | 删除内容 |
|------|----------|
| `p2p/p2p.go` | `sendHeartbeat`、`handleProbe`、`handleProbeReply` |
| `mesh/mesh.go` | `HandleProbe`、`HandleProbeReply`、probe 相关逻辑 |
| `frame/frame.go` | `FrameHeartbeat`、`FrameProbe`、`FrameProbeReply` 常量 |
| `mesh/quality.go` | 基于 probe 的质量追踪（改用 ack 采样） |

---

## 八、兼容性

### 8.1 版本协商

连接建立时通过 hello 交换 ProtocolVersion：
- 版本相同：正常通信
- 版本不同：断开，日志记录

### 8.2 升级策略

需要所有节点同时升级，或支持版本兼容期：
- v6 节点：不识别 seq/ack，按旧格式解析
- v7 节点：检测到对端是 v6，降级为旧模式

**建议**：直接升级，不做兼容（节点数量少，可控）。

---

## 九、实施计划

### 阶段 1：帧格式改造
1. 定义新的控制帧格式（带 seq/ack）
2. 修改 frame 包的读写逻辑
3. 更新 P2PProtocolVersion = 7

### 阶段 2：可靠传输
1. 实现 peerSendState（发送状态 + 重传）
2. 实现 peerRecvState（接收状态 + 去重）
3. 实现 ACK 捎带逻辑

### 阶段 3：链路质量
1. 实现 rttSampler（RTT 采样）
2. 实现 lossTracker（丢包率统计）
3. 替换现有 qualityTracker

### 阶段 4：简化帧类型
1. 删除 heartbeat/probe/probe_reply
2. gossip 周期发送兼做保活
3. 清理相关代码

### 阶段 5：测试验证
1. 单元测试：seq/ack、重传、去重
2. 集成测试：h_tunnel 控制帧可靠送达
3. 链路质量：RTT/丢包率准确性

---

## 十、风险与缓解

| 风险 | 缓解措施 |
|------|----------|
| 重传风暴（网络抖动时大量重传） | 指数退避 + 最大重传次数限制 |
| ACK 延迟导致误判丢包 | 合理设置超时（基于 RTT 动态调整） |
| 去重窗口内存占用 | 限制窗口大小（1000 或 60s） |
| 升级期间版本不兼容 | 所有节点同时升级 |

---

## 附录：与 v6 对比

| 特性 | v6 | v7 |
|------|-----|-----|
| 控制帧可靠 | ❌ | ✅ seq/ack + 重传 |
| 链路质量测量 | probe/probe_reply | ack 被动采样 |
| 帧类型 | 5 种 | 2 种（hello/gossip） |
| 保活机制 | heartbeat（10s） | gossip（15s） |
| 控制帧格式 | {type, payload} | {type, seq, ack, payload} |
