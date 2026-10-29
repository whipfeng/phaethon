# P2P 协议 v8 — 简化控制帧 + Ping/Pong 链路测量

> **文档类型**：Plan  
> **版本**：1.0.0  
> **创建日期**：2026-10-30  
> **最后更新**：2026-10-30

---

## 一、背景与动机

### 1.1 当前问题（v7）

1. **协议复杂度高**：
   - 控制帧（hello、gossip）需要 seq/ack + 重传机制
   - 需要维护 sendState、recvState、pending map 等状态
   - 需要实现指数退避重传、去重窗口等复杂逻辑

2. **Hello 帧时序问题**：
   - 连接建立时发送 hello，但可能因状态转换读到不一致的快照
   - ResendHelloToAll 可能与初次 hello 产生竞态
   - Hello 和 gossip 携带相同信息，存在冗余

3. **ACK 机制过度设计**：
   - Gossip 本身就是定时广播，丢了下次带最新信息
   - 最终一致性足够，不需要可靠传输
   - ACK 机制增加了复杂但没有显著收益

### 1.2 设计目标

1. **简化协议**：
   - 取消 hello 帧和 ack 帧
   - 统一用 gossip 帧广播所有信息
   - 取消可靠传输机制（seq/ack、重传）

2. **保留链路质量测量**：
   - 用专门的 ping/pong 帧测量 RTT 和丢包率
   - 测量的是直连链路的真实质量

3. **消除时序问题**：
   - 所有信息通过 gossip 广播，最终一致性
   - nodeID/subnet 变更会随下次 gossip 传播

---

## 二、协议变更

### 2.1 帧类型

| 类型 | 值 | 说明 | 可靠传输 |
|------|-----|------|----------|
| gossip | 0x12 | 拓扑更新 + 保活 | ❌ |
| ping | 0x14 | 链路质量探测 | ❌ |
| pong | 0x15 | 链路质量响应 | ❌ |
| mesh_packet | 0x20 | 数据帧 | ❌ |

**删除的帧类型**：
- hello（0x11）：功能合并到 gossip
- ack（0x13）：不再需要可靠传输

### 2.2 帧格式

**Gossip 帧**（无需 seq/ack）：
```
┌──────────┬──────────┬─────────────┐
│ type (1) │ len (2)  │ payload ... │
└──────────┴──────────┴─────────────┘
```

| 字段 | 大小 | 说明 |
|------|------|------|
| type | 1 byte | 帧类型（gossip=0x12） |
| len | 2 bytes | payload 长度 |
| payload | 可变 | JSON 编码的 GossipInfo |

**Ping/Pong 帧**：
```
┌──────────┬──────────┬─────────────┐
│ type (1) │ len (2)  │ payload ... │
└──────────┴──────────┴─────────────┘
```

| 字段 | 大小 | 说明 |
|------|------|------|
| type | 1 byte | 帧类型（ping=0x14, pong=0x15） |
| len | 2 bytes | payload 长度 |
| payload | 可变 | JSON 编码的 PingInfo |

**PingInfo 结构**：
```go
type PingInfo struct {
    Timestamp int64  // 发送时间（UnixNano）
    Seq       uint32 // 序列号（用于计算丢包率）
}
```

**数据帧**（mesh_packet）：
- 格式不变，fire-and-forget

### 2.3 协议版本

```go
const P2PProtocolVersion = 8
```

版本变更说明：
- v8：取消 hello/ack，统一 gossip，新增 ping/pong

---

## 三、Gossip 机制

### 3.1 发送时机

1. **定时广播**：每 15 秒发送一次 gossip
2. **连接建立时**：立即发送一次 gossip（不等定时器）
3. **状态变更时**：nodeID/subnet/拓扑变更时立即广播

### 3.2 Gossip 内容

```go
type GossipInfo struct {
    Cmd             string   // "gossip"
    ProtocolVersion int      // 协议版本
    NodeID          string   // 本节点 ID
    Subnet          string   // 本节点 subnet
    VIP             string   // 本节点 VIP
    Routes          []Route  // 路由表
    DomainSuffixes  []string // 域名后缀
    ClaimedSubnets  []string // 通告的子网
}
```

**关键点**：
- 每次 gossip 携带**完整的当前状态**
- 收到 gossip 后更新本地拓扑信息
- 无需确认，丢了下次带最新信息

### 3.3 NodeID vs Subnet 变更的处理

**NodeID 变更**（支持热更新）：
1. 用户修改 nodeID
2. gossipWorker 处理 identityUpdate 事件
3. 更新本地 m.nodeID
4. 立即广播 gossip（携带新 nodeID）
5. 所有邻居收到 gossip，更新拓扑
6. 15 秒后再次广播，确保传播

**Subnet 变更**（需要重启）：
- **不支持热更新**，因为 Subnet 变化会影响大量 gVisor 内部地址：
  - NIC 1 上的 VIP 地址（subnet + 1）
  - NIC 1 上的 GIP 地址（subnet + 3）
  - iptables SNAT 规则中的 VIP
  - RouteSelector 配置（LocalVIP、LocalGIP）
  - FakeIP pool（从 subnet + 10 开始分配）
  - DNSHijacker 的 dnsAddr
- 重建这些地址非常复杂，特别是 FakeIP pool 会影响已有的 DNS 映射
- **建议**：Subnet 变更时，返回 `restart_required` 提示，需要重启生效

**保证**：
- NodeID 变更：最终一致性，所有节点会收到最新的 nodeID
- Subnet 变更：需要重启，重启后使用新 subnet
- 无需可靠传输，无需重传

---

## 四、Ping/Pong 链路质量测量

### 4.1 设计目标

1. **测量 RTT**：往返延迟（毫秒）
2. **测量丢包率**：未响应的 ping 比例
3. **测量对象**：直连邻居（单跳链路）

### 4.2 发送流程

**Ping 发送**：
```go
func (m *P2PManager) sendPing(peer *Peer) {
    ping := PingInfo{
        Timestamp: time.Now().UnixNano(),
        Seq:       peer.nextPingSeq(),
    }
    data, _ := json.Marshal(ping)
    enqueueWrite(peer, frame.FramePing, data, true) // controlCh 高优先级
}
```

**Ping 定时器**：
- 每个 peer 独立定时器，每 1 秒发送一次 ping
- Ping 走 controlCh 高优先级队列

**Pong 响应**：
```go
func (m *P2PManager) handlePing(peer *Peer, payload []byte) {
    var ping PingInfo
    json.Unmarshal(payload, &ping)
    
    // 立即回复 pong，携带相同的时间戳
    pong := PingInfo{
        Timestamp: ping.Timestamp,
        Seq:       ping.Seq,
    }
    data, _ := json.Marshal(pong)
    enqueueWrite(peer, frame.FramePong, data, true)
}
```

**RTT 计算**：
```go
func (m *P2PManager) handlePong(peer *Peer, payload []byte) {
    var pong PingInfo
    json.Unmarshal(payload, &pong)
    
    rtt := time.Now().UnixNano() - pong.Timestamp
    peer.rttSampler.Add(time.Duration(rtt))
}
```

### 4.3 丢包率计算

```go
type pingTracker struct {
    pendingPings map[uint32]time.Time // seq -> 发送时间
    totalSent    uint64
    totalReceived uint64
}

// 发送 ping 时
tracker.pendingPings[seq] = time.Now()
tracker.totalSent++

// 收到 pong 时
if _, ok := tracker.pendingPings[seq]; ok {
    delete(tracker.pendingPings, seq)
    tracker.totalReceived++
}

// 定期清理超时 ping（5 秒未响应）
for seq, sentTime := range tracker.pendingPings {
    if time.Since(sentTime) > 5*time.Second {
        delete(tracker.pendingPings, seq)
    }
}

// 丢包率 = 1 - (totalReceived / totalSent)
lossRate := 1.0 - float64(tracker.totalReceived) / float64(tracker.totalSent)
```

### 4.4 链路质量评估

```go
type LinkQuality struct {
    RTT       time.Duration // 平均 RTT
    LossRate  float64       // 丢包率 (0.0 - 1.0)
    Jitter    time.Duration // RTT 抖动
    Confidence float64      // 置信度 (0.0 - 1.0)
}
```

**用途**：
- 路由选择：优先选择 RTT 低、丢包率低的链路
- 故障检测：连续多次 ping 无响应，判定链路断开

---

## 五、连接建立流程

### 5.1 简化后的流程

**v7 流程**：
1. DialP2P 建立传输层连接
2. 发送 hello（携带 gossip 信息）
3. 对端收到 hello，注册 peer
4. 对端回复 hello
5. 开始定时 gossip

**v8 流程**：
1. DialP2P 建立传输层连接
2. **立即发送 gossip**（携带完整拓扑信息）
3. 对端收到 gossip，注册 peer，更新拓扑
4. 对端回复 gossip（携带自己的拓扑信息）
5. 开始定时 gossip + ping/pong

### 5.2 关键变化

1. **取消 hello 帧**：连接建立后直接发 gossip
2. **取消握手**：无需双向 hello，单向 gossip 即可
3. **立即生效**：收到第一个 gossip 就注册 peer 并更新拓扑

### 5.3 Peer 注册

**Register 时机**：
1. 收到**第一个 gossip 帧**（peer.NodeID 为空）
2. 从 gossip 中提取 NodeID
3. 创建 peerSender，调用 `meshHandler.RegisterPeer(sender)`
4. 启动 ping 定时器

**Unregister 时机**：
1. 连接断开（transport 关闭）
2. defer 中调用 `meshHandler.UnregisterPeer(sender)`

**NodeID 变化处理**：
```go
func (m *P2PManager) handleGossip(peer *Peer, info GossipInfo) {
    // 首次收到 gossip = 建交
    if peer.NodeID == "" {
        peer.NodeID = info.NodeID
        ps := &peerSender{peer: peer, nodeID: info.NodeID}
        peer.SetMeshSender(ps)
        m.meshHandler.RegisterPeer(ps)
        peer.startPingTimer()
    } else if peer.NodeID != info.NodeID {
        // NodeID 变化，重新注册
        util.LogModuleInfo("p2p", "[P2P] peer %s nodeID changed: %s -> %s", 
            peer.ID, peer.NodeID, info.NodeID)
        
        oldSender := peer.GetMeshSender()
        if oldSender != nil {
            m.meshHandler.UnregisterPeer(oldSender)
        }
        
        peer.NodeID = info.NodeID
        ps := &peerSender{peer: peer, nodeID: info.NodeID}
        peer.SetMeshSender(ps)
        m.meshHandler.RegisterPeer(ps)
    }
    
    // 更新拓扑（包括 Subnet 变化）
    m.meshHandler.ProcessGossip(peer.meshSender, info)
}
```

**Subnet 变化处理**：
- Subnet 变化**不需要重新注册**（peerSender 只关心 NodeID）
- `ProcessGossip` 会更新拓扑信息（包括 Subnet、Routes 等）
- 路由会自动重新计算

---

## 六、控制帧优先级

### 6.1 队列设计

保留 controlCh 高优先级队列：

```go
type Peer struct {
    controlCh   chan writeReq // 高优先级：gossip/ping/pong
    dnsQueue    chan writeReq // 次高优先级：DNS 包
    ackQueue    chan writeReq // 高优先级：TCP ACK
    dataQueues  [256]chan writeReq // 普通优先级：数据帧
}
```

**优先级顺序**：
1. controlCh（gossip、ping、pong）
2. dnsQueue（DNS 包）
3. ackQueue（TCP ACK）
4. dataQueues（数据帧，DRR 调度）

### 6.2 为什么保留高优先级

1. **控制帧重要性**：
   - gossip 携带拓扑信息，影响路由决策
   - ping/pong 用于链路质量测量，影响路径选择

2. **避免拥塞**：
   - 数据帧可能很多，控制帧应该优先发送
   - 确保拓扑更新和链路测量不受数据流量影响

---

## 七、删除的代码

| 文件 | 删除内容 |
|------|----------|
| `p2p/p2p.go` | `sendHello`、`handleHello`、seq/ack 逻辑、sendState、recvState、重传机制 |
| `p2p/reliable.go` | 整个文件（不再需要可靠传输） |
| `frame/frame.go` | `FrameHello`、`FrameAck` 常量 |
| `mesh/mesh.go` | `ResendHelloToAll`（改为 `BroadcastGossip`） |

---

## 八、兼容性

### 8.1 版本协商

连接建立后，第一个 gossip 帧携带 ProtocolVersion：
- 版本相同：正常通信
- 版本不同：断开，日志记录

### 8.2 升级策略

**建议**：所有节点同时升级到 v8，不做版本兼容。

**原因**：
- 节点数量少，可控
- v7 和 v8 协议格式不兼容（帧类型、格式都变了）
- 做兼容会增加复杂度

---

## 九、实施计划

### 阶段 1：帧格式改造
1. 删除 FrameHello、FrameAck 常量
2. 新增 FramePing、FramePong 常量
3. 修改控制帧格式（去掉 seq/ack）
4. 更新 P2PProtocolVersion = 8

### 阶段 2：简化控制帧
1. 删除 sendHello、handleHello
2. 连接建立后直接发 gossip
3. 删除 seq/ack 逻辑、sendState、recvState
4. 删除重传机制

### 阶段 3：实现 Ping/Pong
1. 实现 sendPing、handlePing、handlePong
2. 实现 pingTracker（丢包率统计）
3. 实现 ping 定时器（每 peer 1 秒）
4. 更新 rttSampler（基于 ping/pong）

### 阶段 4：清理代码
1. 删除 reliable.go
2. 删除 ResendHelloToAll（改为 BroadcastGossip）
3. 更新日志和注释

### 阶段 5：测试验证
1. 单元测试：gossip 广播、ping/pong 测量
2. 集成测试：连接建立、拓扑更新
3. 链路质量：RTT/丢包率准确性

---

## 十、风险与缓解

| 风险 | 缓解措施 |
|------|----------|
| Gossip 丢失导致拓扑不一致 | 定时广播（15s），最终一致性保证 |
| Ping/Pong 丢失导致链路质量不准 | 滑动窗口平均，多次采样 |
| 升级期间版本不兼容 | 所有节点同时升级 |
| Ping 风暴（大量 peer） | 限制 ping 频率（1s），控制帧走 controlCh |

---

## 附录：与 v7 对比

| 特性 | v7 | v8 |
|------|-----|-----|
| 控制帧类型 | hello、gossip、ack | gossip、ping、pong |
| 可靠传输 | ✅ seq/ack + 重传 | ❌ 最终一致性 |
| 链路质量测量 | ack 被动采样 | ping/pong 主动探测 |
| 连接建立 | 双向 hello 握手 | 单向 gossip |
| 协议复杂度 | 高（sendState、recvState、重传） | 低（无状态广播） |
| 控制帧格式 | {type, seq, ack, payload} | {type, payload} |
| 时序问题 | hello/gossip 竞态 | 无（统一 gossip） |

---

## 总结

v8 的核心思想是**简化协议，依赖最终一致性**：

1. **取消可靠传输**：gossip 丢了就丢了，下次带最新信息
2. **统一信息传播**：所有拓扑信息通过 gossip 广播，没有 hello/gossip 的区分
3. **专门的链路测量**：ping/pong 帧用于测量 RTT 和丢包率，不影响 gossip 机制
4. **消除时序问题**：nodeID/subnet 变更随 gossip 传播，最终一致

这样既简化了协议实现，又保证了功能的完整性。
