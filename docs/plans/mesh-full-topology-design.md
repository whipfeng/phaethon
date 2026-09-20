# Mesh Gossip 协议优化与全网拓扑设计方案

## 背景

### 原始需求
当前 Mesh 拓扑图只显示本地节点的直接邻居（一跳范围）。用户希望看到全网视图，了解完整的网络拓扑结构。

### 发现的问题
在实现全网拓扑功能时，发现了现有 gossip 协议的严重问题：

1. **过时路由游荡问题**：当节点停止通告某个路由/subnet后，其他节点存储的旧信息不会被清除，继续在网络中传播，每次 hop+1，导致 hop count 累积到 400+
2. **字段冗余问题**：GossipInfo 中存在多个冗余字段，增加了协议复杂性和报文大小

## 目标

- 解决过时路由游荡问题
- 消除协议冗余，简化设计
- 每个节点能够展示完整的 Mesh 网络拓扑
- 显示所有节点及其连接关系
- 支持故障排查（查看路由路径）
- 复用现有 gossip 机制，不引入新的消息类型

## 问题分析

### 过时路由游荡问题

**场景**：节点A原来有subnet X，后来改成Y

1. A通告X给B（hop=0）
2. B存储并传播给C（hop=1）
3. C存储并传播给B（hop=2，但split horizon阻止回传给B）
4. A改成Y，停止通告X
5. B更新A.PeerInfo，不再包含X
6. **但是** B的bestClaims还会从C.PeerInfo中包含X（hop=2）
7. B继续传播X给其他节点，hop count不断累积

**根本原因**：当前协议没有路由老化机制，一旦学到某个路由就会一直保留并传播。

### 字段冗余分析

| 字段 | 冗余？ | 原因 |
|------|--------|------|
| NodeID | ✅ 冗余 | 发送方身份已通过PeerSender识别 |
| Subnet | ✅ 冗余 | 可从ClaimedSubnets中hop=0的条目推导 |
| Routes.Hop | ✅ 冗余 | 路由所有者的距离已在ClaimedSubnets中维护 |
| DomainSuffixes.Hop | ✅ 冗余 | 域名后缀所有者的距离已在ClaimedSubnets中维护 |
| DomainSuffixes.Subnet | ✅ 冗余 | 域名后缀所有者的subnet已在ClaimedSubnets中维护 |
| TopologyEdges | ✅ 冗余 | 可通过在ClaimedSubnets中添加Neighbors字段表达 |

## 设计方案

### 核心思路

1. **简化GossipInfo**：移除冗余字段
2. **引用式设计**：Routes和DomainSuffixes引用nodeID，hop count从ClaimedSubnets推导
3. **合并拓扑信息**：在ClaimedSubnets中添加Neighbors字段，替代TopologyEdges

### 新的数据结构

```go
// GossipInfo 简化后的结构
type GossipInfo struct {
    // 移除 NodeID 字段（通过PeerSender识别）
    // 移除 Subnet 字段（从ClaimedSubnets推导）
    
    DomainSuffixes []GossipDomainSuffix    `json:"domainSuffixes,omitempty"`
    Routes         []GossipRoute           `json:"routes,omitempty"`
    ClaimedSubnets []GossipClaimedSubnet   `json:"claimedSubnets,omitempty"`
    // 移除 TopologyEdges 字段（通过ClaimedSubnets.Neighbors表达）
}

// GossipRoute 简化：只引用所有者，不维护hop count
type GossipRoute struct {
    Prefix string `json:"prefix"`
    NodeID string `json:"nodeId"`  // 路由所有者
}

// GossipDomainSuffix 简化：只引用所有者，不维护hop count和subnet
type GossipDomainSuffix struct {
    Suffix string `json:"suffix"`
    NodeID string `json:"nodeId"`  // 域名后缀所有者
}

// GossipClaimedSubnet 增强：添加Neighbors字段表达拓扑
type GossipClaimedSubnet struct {
    Subnet    string   `json:"subnet"`
    NodeID    string   `json:"nodeId"`
    Hop       int      `json:"hop"`
    Neighbors []string `json:"neighbors,omitempty"`  // 该节点直连的节点列表
}
```

### 报文示例

```json
{
  "claimedSubnets": [
    {
      "subnet": "100.179.0.0/16",
      "nodeId": "gg",
      "hop": 0,
      "neighbors": ["ms9", "ms10"]
    },
    {
      "subnet": "100.189.0.0/16",
      "nodeId": "ms9",
      "hop": 1,
      "neighbors": ["gg", "x"]
    },
    {
      "subnet": "100.96.0.0/16",
      "nodeId": "ms10",
      "hop": 1,
      "neighbors": ["gg"]
    }
  ],
  "routes": [
    {"prefix": "192.168.1.0/24", "nodeId": "gg"},
    {"prefix": "10.10.0.0/16", "nodeId": "ms9"}
  ],
  "domainSuffixes": [
    {"suffix": ".internal", "nodeId": "gg"},
    {"suffix": ".ms9.internal", "nodeId": "ms9"}
  ]
}
```

### 处理逻辑

#### 发送方（broadcastGossip）

1. **构建ClaimedSubnets**：
   - 自己的条目：`{subnet: m.subnetStr, nodeID: m.nodeID, hop: 0, neighbors: [所有直连peer的nodeID]}`
   - 从peer学到的条目：保留原hop count和neighbors，hop+1

2. **构建Routes**：
   - 自己的路由：`{prefix: r, nodeID: m.nodeID}`
   - 从peer学到的路由：`{prefix: r.Prefix, nodeID: r.NodeID}`（保留原nodeID）

3. **构建DomainSuffixes**：
   - 自己的后缀：`{suffix: s, nodeID: m.nodeID}`
   - 从peer学到的后缀：`{suffix: entry.Suffix, nodeID: entry.NodeID}`（保留原nodeID）

4. **Split horizon过滤**：
   - 对每个peer，过滤掉nextHop==该peer的条目
   - 使用nodeID比较而非指针比较，更可靠

#### 接收方（UpdateGossip）

1. **解析ClaimedSubnets**：
   - 建立nodeID到hop count的映射
   - 建立nodeID到neighbors的映射
   - 建立nodeID到subnet的映射

2. **解析Routes**：
   - 查找route.NodeID的hop count
   - 路由的实际hop = route.NodeID的hop count

3. **解析DomainSuffixes**：
   - 查找suffix.NodeID的hop count和subnet
   - 后缀的实际hop = suffix.NodeID的hop count
   - 后缀的subnet = suffix.NodeID的subnet

4. **构建全网拓扑**：
   - 从所有ClaimedSubnets的Neighbors字段推导边
   - 例如：A.neighbors=[B,C] → 边(A,B), (A,C)

### 解决过时路由游荡问题

#### 问题场景

当节点下线后，它的 ClaimedSubnet 可能继续在网络中游荡：

**拓扑：A-B-C 成环，D 连 A，D 下线**

1. D 在线时：D 通告 `{nodeID: "D", neighbors: ["A"]}`，A/B/C 都存储
2. D 下线：A 检测到断开，删除 D 的 peer 条目，停止通告 D 的 claim
3. **但 B 和 C 还存着 D 的 claim**（从 A 学来的）
4. B 和 C 互相转发 D 的 claim，形成环路游荡

#### 解决方案：互相声明验证

**规则**：如果节点 X 声明邻居包含 [Y, Z, ...]，那么 Y、Z 等**所有**邻居都必须也声明 X。否则 X 的 claim 无效，丢弃。

**验证逻辑**：当你收到一个 claim 时，检查 claim 的 origin 声明的**每一个** neighbor 是否也声明了 origin。如果有任何一个 neighbor 不再声明 origin（或 neighbor 本身不存在），则该 claim 无效。

```go
// 验证 claim 的有效性：必须所有邻居都通过验证
for _, cs := range claimedSubnets {
    originID := cs.NodeID
    originNeighbors := cs.Neighbors  // origin 声明的邻居列表
    
    // 检查：origin 的每个邻居是否也声明了 origin
    validated := true
    for _, neighborID := range originNeighbors {
        neighborClaim, ok := findClaim(neighborID)
        if !ok {
            // neighbor 不存在（下线了）→ 验证失败
            validated = false
            break
        }
        if !contains(neighborClaim.neighbors, originID) {
            // neighbor 存在但没有声明 origin → 验证失败
            validated = false
            break
        }
    }
    if !validated {
        continue  // 丢弃无效 claim
    }
}
```

**关键点**：
- **全部检查**：必须 origin 的**所有**邻居都声明 origin，claim 才有效
- **等价逻辑**：neighbor 不存在（下线）和 neighbor 存在但不声明 origin，在验证逻辑看来是等价的——都导致验证失败
- **严格验证**：只要有一个邻居不满足，整个 claim 就无效。这确保 neighbors 列表的准确性

**场景追踪**：

D 在线时：
- D 通告：`{nodeID: "D", neighbors: ["A"]}`
- A 通告：`{nodeID: "A", neighbors: ["B", "D"]}`（包含 D）
- B 收到 D 的 claim：检查 D.Neighbors=["A"]，A 在通告 D 的 claim → ✓ 有效
- C 收到 D 的 claim：同样检查，A 在通告 → ✓ 有效

D 下线后：
- A 检测到断开，A 的 Neighbors 变成 `["B"]`（不再包含 D）
- A 停止通告 D 的 claim
- B 收到 D 的 claim（从 C 转发）：检查 D.Neighbors=["A"]，A **不再通告** D 的 claim → ✗ **无效，丢弃！**
- C 同样丢弃
- D 的 claim 在一轮 gossip 内从全网消失 ✓

**多跳传播**：

拓扑 A-B-C-D-E，A 的 claim: neighbors=["B"]

- B 收到 A 的 claim：检查 A.Neighbors=["B"]，B 在通告 A 的 claim → ✓ 有效
- C 收到 A 的 claim：同样检查，B 在通告 → ✓ 有效
- D 收到 A 的 claim：同样检查，B 在通告 → ✓ 有效
- E 收到 A 的 claim：同样检查，B 在通告 → ✓ 有效

Claim 自由传播，因为验证的是 claim 内容本身（A 和 B 的互相声明关系），不是检查 sender。

A 下线后：
- B 检测到断开，B 的 Neighbors 不再包含 A
- B 停止通告 A 的 claim
- C 收到 A 的 claim：检查 A.Neighbors=["B"]，B **不再通告** → ✗ 无效，丢弃
- D、E 同样丢弃
- 全网清理 ✓

**优势**：
- 不需要 TTL/时间戳
- 不需要新消息类型
- 纯拓扑推理，自动清理 stale claims
- 支持多跳传播
- 逻辑统一，不检查 sender，只验证 claim 内容

### 全网拓扑推导

从ClaimedSubnets推导拓扑边：

```go
// 从ClaimedSubnets推导拓扑
edgeSet := make(map[string]FullTopologyEdge)

for _, cs := range allClaimedSubnets {
    for _, neighbor := range cs.Neighbors {
        key := edgeKey(cs.NodeID, neighbor)
        edgeSet[key] = FullTopologyEdge{From: cs.NodeID, To: neighbor}
    }
}
```

**示例**：
- gg.neighbors=[ms9, ms10] → 边(gg,ms9), (gg,ms10)
- ms9.neighbors=[gg, x] → 边(ms9,gg), (ms9,x)
- ms10.neighbors=[gg] → 边(ms10,gg)

去重后得到全网拓扑。

## 实现步骤

1. **协议层**（已完成）：
   - 修改GossipInfo结构，移除NodeID和Subnet字段
   - 修改GossipRoute和GossipDomainSuffix，移除Hop字段，添加NodeID字段
   - 修改GossipClaimedSubnet，添加Neighbors字段
   - 移除GossipTopologyEdge结构
   - **P2PProtocolVersion 从 2 升到 3**（协议不兼容，旧版本节点会拒绝连接）

2. **发送逻辑**（已完成）：
   - 修改broadcastGossip()，构建新的报文格式
   - 自己的ClaimedSubnet条目包含Neighbors
   - Routes和DomainSuffixes引用nodeID

3. **接收逻辑**：
   - 修改UpdateGossip()，解析新格式
   - **添加互相声明验证**：检查 claim 的 origin 声明的 neighbors 是否也声明了 origin
   - 建立nodeID到hop/subnet/neighbors的映射
   - Routes和DomainSuffixes的hop从映射中查找

4. **聚合逻辑**：
   - 在broadcastGossip()聚合时，对每个claim执行互相声明验证
   - 无效的claim（origin的某个neighbor不再声明origin）不纳入bestClaims

5. **拓扑展示**：
   - 修改GetFullTopology()，从ClaimedSubnets.Neighbors推导边
   - 移除对TopologyEdges的依赖

6. **测试验证**：
   - 验证节点下线后claim自动清理
   - 验证全网拓扑正确展示
   - 验证split horizon正常工作
   - 验证多跳传播正常

## 优势

1. **解决游荡问题**：过时路由自动清理，不会累积到400+ hop
2. **消除冗余**：减少字段重复，简化协议
3. **减小报文**：移除冗余字段，减少网络开销
4. **逻辑清晰**：引用式设计，关系明确
5. **自动一致性**：节点信息变更时，相关路由和拓扑自动更新

## 状态

- [x] 问题分析完成
- [x] 冗余分析完成
- [x] 新设计完成
- [x] 互相声明验证规则设计
- [ ] 实现互相声明验证
- [ ] 实现 recomputeRoutes 更新
- [ ] 实现 GetFullTopology 更新
- [ ] 测试验证
