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
const P2PProtocolVersion = 10
```

版本变更说明：
- v8：取消 hello/ack，统一 gossip，新增 ping/pong（设计初版使用的版本号，未正式发布）
- v9：GossipLink 改为 localSeq/remoteSeq 字段配对识别链路（已随旧版本发布，线上存在）
- v10：v8 简化协议正式发布。**跳过 9 直接定为 10**，因为 v9 已占用版本号 9 且 gossip 中 GossipLink 格式不同——若新协议也用 9，线上旧节点会把新协议当作同版本接受

**版本协商（强制）**：收到第一个 gossip 时比对 `ProtocolVersion`：
- 版本相同：正常通信
- 版本不同：**断开连接并记 Warn 日志**（§8.1）。禁止静默接受版本不匹配的连接。

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
    Seq             uint16   // 本端链路序列号；对端用它设置 RemoteSeq 并计算 LinkID（见 §5.3）
    Routes          []Route  // 路由表
    DomainSuffixes  []string // 域名后缀
    ClaimedSubnets  []string // 通告的子网
}
```

**关键点**：
- 每次 gossip 携带**完整的当前状态**
- 收到 gossip 后更新本地拓扑信息
- 无需确认，丢了下次带最新信息
- **发送时必须填充 Seq**（本端链路序列号）；首个 gossip 的 Seq 用于对端协商 RemoteSeq/LinkID（§5.3），mesh 的 GossipLink 配对依赖它

### 3.3 NodeID vs Subnet 变更的处理

**NodeID 与 Subnet 变更（2026-10-30 最终决策：统一重启生效）**：

用户决策：nodeID 与 subnet 修改统一走"**保存配置 + 重启生效**"，重启依赖各环境进程级自动拉起（VM 内置 watchdog、QG OpenRC supervise-daemon、GG/MS9/MS10 watchdog 模式），无需人工介入，重启成本秒级。

**为什么 nodeID 也不做热更新**（此前设计为热更新，2026-10-30 修订）：
- nodeID 虽不像 subnet 那样烧入地址体系（VIP/GIP/SNAT/FakeIP 均为 subnet 派生），但也有身份烧入点：`ConnectionHandler.localMeshNodeID` 启动缓存（main_engine.go:176 创建后无刷新机制）、domainTrie "own suffixes → self" 静态条目（mesh.go:1858）、mTLS 客户端证书 CN 校验（CN=nodeID 的证书在改名后失效）
- 热更需逐项刷新这些缓存，复杂且易漏——v8 初版实现就只更新了 p2p.SetMeshInfo 一处
- 该操作低频，重启成本远低于热更的实现与维护成本
- **收益**：消除了 identityUpdate 运行态分支引入的数据竞争（issues/p2p-v8-mesh-regression.md 问题4）和 ChangeIdentity 事件静默丢弃风险（问题10）

**行为**（2026-10-30 修订：保存即重启，用户决策）：
1. 前端检测到 node-id/subnet 被修改 → 弹确认框"保存后节点将自动重启"，**取消则不保存**
2. 确认后 admin API 保存新 nodeID/subnet 到数据库（**保存前清空 DB fakeip bucket**——subnet 变化后旧映射虽被 LoadFromDB 的 Contains 过滤，但残留在 DB 里是垃圾），返回 200 + `restart_required`
3. 响应发出后进程自我退出（exit 0），由 watchdog/supervise-daemon 自动拉起（与 subnet 冲突自动重分配同一机制）
4. 拉起后加载新身份，FakeIP 池从空重建（nextIP 回到 subnet+skip+1），邻居经 gossip 收敛（约 15 秒）

**Subnet 变更的具体影响**（重启的根本原因）——Subnet 变化会影响大量 gVisor 内部地址：
  - NIC 1 上的 VIP 地址（subnet + 1）
  - NIC 1 上的 GIP 地址（subnet + 3）
  - iptables SNAT 规则中的 VIP
  - RouteSelector 配置（LocalVIP、LocalGIP）
  - FakeIP pool（从 subnet + 10 开始分配）
  - DNSHijacker 的 dnsAddr
- 重建这些地址非常复杂，特别是 FakeIP pool 会影响已有的 DNS 映射

> **实现红线（2026-10-30 修订）**：nodeID/subnet 变更**禁止热生效**。ffbf566 曾实现为直接热改 `m.subnet/m.vip` 并全网广播，但不重建本地 NIC 地址/SNAT/RouteSelector/FakeIP/DNSHijacker，导致对端按新 VIP 发包而本机不认领的流量黑洞（详见 `docs/issues/p2p-v8-mesh-regression.md` 问题3）。正确行为：admin 保存 → 确认重启 → 存库 + 进程自我退出 + watchdog 拉起新身份，全程无热改。

**subnet 冲突自动重分配**（27d817f 的配套语义）：
1. 检测到冲突 → 选取新 subnet 保存数据库
2. 记录日志后**进程自我退出**（exit 0），由 watchdog/supervise-daemon 自动拉起
3. 拉起后使用新 subnet，冲突解除
- 目的：避免两个节点在无人干预的情况下长时间同时通告冲突 subnet

**实现清理项**（随本决策）：
- 删除 gossipWorker 的 identityUpdate 运行态分支（mesh.go 约 2237-2301）——无运行态身份变更后该分支是死代码兼竞争源
- `UpdateIdentity`（mesh.go:757）本就已零调用，一并删除
- `ChangeIdentity` 改为同步：存库 + 返回是否需要重启，不再投递 normalCh
- `ConnectionHandler` 无需新增 nodeID setter（此前为热更准备）

**保证**：
- nodeID/subnet 变更：保存成功 + 重启提示，重启后全网经 gossip 收敛
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

**滑动窗口统计**（2026-10-30 修订，原"会话累计"方案会让高 RTT 链路丢包率永久虚高并毒化选路）：

```go
type pingTracker struct {
    pendingPings map[uint32]time.Time // seq -> 发送时间
    window       []pingResult         // 最近 N 次 ping 的结果（N=20）
}

type pingResult struct {
    seq      uint32
    sent     time.Time
    received bool
}

// 发送 ping 时：仅入队 pending，不预记 sent
tracker.pendingPings[seq] = time.Now()

// 发送成功（enqueueWrite 返回 nil）后才记窗口
// 发送失败：从 pendingPings 删除，不计入统计（避免虚增 sent）

// 收到 pong 时
if _, ok := tracker.pendingPings[seq]; ok {
    delete(tracker.pendingPings, seq)
    tracker.window = append(tracker.window, pingResult{seq: seq, received: true})
}

// 定期清理超时 ping（5 秒未响应）：记为丢包入窗口后删除
for seq, sentTime := range tracker.pendingPings {
    if time.Since(sentTime) > 5*time.Second {
        delete(tracker.pendingPings, seq)
        tracker.window = append(tracker.window, pingResult{seq: seq, received: false})
    }
}

// 窗口截断到最近 N 次
if len(tracker.window) > 20 {
    tracker.window = tracker.window[len(tracker.window)-20:]
}

// 丢包率 = 窗口内未收到 pong 的比例（链路恢复后能随窗口滑动而回落）
var lost int
for _, r := range tracker.window {
    if !r.received { lost++ }
}
lossRate := float64(lost) / float64(len(tracker.window))
```

**修订要点**：
- 发送失败（enqueueWrite 报错）不计入统计
- 超时清理时把该 ping 记为"丢包"入窗口（而非原方案的只删不回滚）
- 只统计最近 20 次，链路恢复后 lossRate 能回落

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
- 故障检测：**连续 10 个 ping 未收到 pong（约 10 秒）判定链路死**（2026-10-30 决策，由"后续增强"改为本次实现）：
  1. pingTracker 记录连续未应答计数；ping 发送失败（enqueueWrite 报错，队列塞死本身即濒死症状）计入未应答
  2. 达到阈值：记 Warn 日志（含 peer 标识、连续丢失数、当前 lossRate）并关闭 transport
  3. 走现有断开路径：UnregisterPeer → 路由重算 → 拨号器重连
  4. 30 秒全静默 heartbeat 保留作为兜底（覆盖对端彻底失联、gossip 也停的场景）
  5. 断链判定后可考虑清零 pingTracker 窗口，避免旧统计影响重连后的质量评估

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

**链路标识协商（2026-10-30 补充）**：
1. gossip 帧携带发送方本地 Seq（`GossipInfo.Seq`）
2. 收到首个 gossip 时：`peer.RemoteSeq = info.Seq`，`peer.LinkID = fmt.Sprintf("%d-%d", min(本地Seq, 对端Seq), max(本地Seq, 对端Seq))`
3. mesh 侧 GossipLink 的 localSeq/remoteSeq 配对（反向链路匹配、按 linkID 查询/停链路）依赖此协商结果

> **实现警示（2026-10-30）**：上述注册逻辑**必须在 handleGossip 的首个 gossip 分支中实现**。v8 初版实现（2a0589e）删除 handleHello 时连带删除了注册调用且未按本节重建，导致 `GetMeshSender()` 恒 nil、gossip 被静默丢弃、mesh 数据面整体失效（详见 `docs/issues/p2p-v8-mesh-regression.md` 问题1）。删除旧代码时必须逐项核对被删代码承担的职责是否在设计中有替代方案。

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

**实现要求**：handleGossip 收到首个 gossip 必须执行此检查，不匹配则 `transport.Close()` + Warn 日志。禁止静默接受版本不匹配的连接（v8 初版实现遗漏此检查，2026-10-30 修复）。

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

## 变更记录

- **2026-10-30**：根据代码审查结论（`docs/issues/p2p-v8-mesh-regression.md`）修订，决策人为用户本人：
  1. 协议版本 8 → **10**（避让线上 v9 节点），版本协商升级为强制检查（§2.3、§8.1）
  2. `GossipInfo` 补充 `Seq` 字段，明确链路标识协商流程（§3.2、§5.3）——gossip 帧携带本端 Seq，对端首个 gossip 推导 RemoteSeq/LinkID
  3. §3.3 增加"Subnet 变更禁止热生效"实现红线，恢复 `restart_required` 语义；subnet 冲突自动重分配只改库不热改
  4. §4.3 丢包率改为滑动窗口统计（最近 20 次）+ 发送失败不计入 + 超时清理记为丢包，替代原会话累计方案
  5. §4.4 故障检测（连续 10 ping 无 pong 断链）**由"后续增强"改为本次实现**，给出阈值、动作与 heartbeat 兜底关系
  6. §3.3 **NodeID 变更由热更新改为重启生效**（用户三次决策）：与 subnet 统一"保存配置 + restart_required + 看门狗自动拉起"；冲突自动重分配改为存库 + 自我退出；identityUpdate 运行态分支列为删除项；问题4（竞争）与问题10（事件丢弃）随此决策消解
- **2026-10-30（第二批）**：身份变更"保存即重启"完整落地（用户决策）：
  1. §3.3 行为定稿为"前端确认 → 存库 + 清 fakeip bucket → 返回 `restart_required` → 进程自我退出（exit 0）→ watchdog 拉起"，FakeIP 池随重启从空重建
  2. admin API 新增 `RequestRestart` 回调钩子（响应发出后触发）；前端 mesh.html 在 node-id/subnet 变更时弹确认框，**取消则不保存**
  3. VM 端到端验证通过：PUT 身份变更 → 200 + `restart_required` → 进程退出 → watchdog 拉起新身份 → 回滚同样生效

这样既简化了协议实现，又保证了功能的完整性。
